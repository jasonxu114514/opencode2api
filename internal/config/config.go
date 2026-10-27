// Package config loads, validates, persists, and redacts service configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/warp"
)

// Config is the service configuration stored in config.json.
//
// ServerKeys are the local keys clients present to the gateway; ZenKeys and
// GoKeys are the upstream keys of the two keyed tiers, and Anonymous enables
// the keyless free tier. Proxies lists the configured proxy entries; the
// entries read from ProxyFile join them only in the resolved list that
// RuntimeProxies returns, which is never saved. Prefer picks the keyed tier
// that is tried first when both serve a model.
type Config struct {
	Rotation          RotationConfig               `json:"rotation"`
	Listen            string                       `json:"listen"`
	ServerKeys        []string                     `json:"server_keys"`
	ServerKeyMetadata map[string]ServerKeyMetadata `json:"server_key_metadata,omitempty"`
	ZenKeys           []string                     `json:"zen_keys"`
	GoKeys            []string                     `json:"go_keys"`
	Anonymous         bool                         `json:"anonymous"`
	Warp              WarpConfig                   `json:"warp"`
	Proxies           []string                     `json:"proxies"`
	ProxyFile         string                       `json:"proxyfile"`
	Upstream          UpstreamConfig               `json:"upstream"`
	Retry             RetryConfig                  `json:"retry"`
	Models            ModelsConfig                 `json:"models"`
	Performance       PerformanceConfig            `json:"performance"`
	Logging           LoggingConfig                `json:"logging"`
	WebUI             WebUIConfig                  `json:"webui"`
	Prefer            Tier                         `json:"prefer"`
	Reasoning         ReasoningConfig              `json:"reasoning"`

	effectiveProxies []string
}

// ReasoningConfig forces a thinking level for requests that do not state one.
//
// Effort is the default for every model; EffortByModel overrides it for the
// model IDs it names. A level a client sends explicitly (Anthropic's
// output_config.effort / top-level effort, Chat's reasoning_effort, or
// Responses' reasoning.effort) always wins, so a forced level only replaces a
// value that would otherwise have been derived from thinking.budget_tokens or
// left unset. Besides the levels themselves, "none" strips reasoning from the
// request, and "auto" applies, for free models on the Zen tier only, the
// highest level the availability probe found the model to accept.
type ReasoningConfig struct {
	// omitempty keeps a saved configuration free of the fields the operator
	// never set, matching how the rest of the optional surface is persisted.
	Effort        string            `json:"effort,omitempty"`
	EffortByModel map[string]string `json:"effort_by_model,omitempty"`
}

// WarpConfig controls the optional Cloudflare WARP egress. While enabled,
// free-model traffic on the anonymous lane and the free Zen-key fallback is
// sent through a userspace WireGuard tunnel instead of any configured proxy.
// PoolSize is the number of registered WARP exits kept warm (the last one is
// held as hot standby); Stack selects the tunnel's address family. Initialization
// is fail-closed: a configuration that enables WARP is rejected when no exit
// can be established.
type WarpConfig struct {
	Enabled  bool   `json:"enabled"`
	PoolSize int    `json:"pool_size,omitempty"`
	Stack    string `json:"stack,omitempty"`
	// SourceIPv6 is an outer IPv6 prefix (usually a routed /64) each WARP exit
	// binds its WireGuard socket to. Rotating the source is what gives a
	// replacement exit a new public egress; without it, exits share one
	// address and an egress-scoped free quota stays exhausted.
	SourceIPv6 string `json:"source_ipv6,omitempty"`
}

// Normalize fills the defaults and canonicalizes the stack name.
func (w *WarpConfig) Normalize() error {
	if w.PoolSize == 0 {
		w.PoolSize = 2
	}
	if strings.TrimSpace(w.Stack) == "" {
		w.Stack = string(warp.DefaultStack)
	}
	stack, err := warp.ParseStack(w.Stack)
	if err != nil {
		return fmt.Errorf("warp.stack: %w", err)
	}
	w.Stack = string(stack)
	if w.PoolSize < 1 || w.PoolSize > 32 {
		return errors.New("warp.pool_size must be between 1 and 32")
	}
	w.SourceIPv6 = strings.TrimSpace(w.SourceIPv6)
	if w.SourceIPv6 != "" {
		prefix, err := netip.ParsePrefix(w.SourceIPv6)
		if err != nil || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
			return fmt.Errorf("warp.source_ipv6: %q is not an IPv6 prefix", w.SourceIPv6)
		}
		if prefix.Bits() > 64 {
			return fmt.Errorf("warp.source_ipv6: %s must be /64 or shorter", prefix)
		}
		if stack != warp.StackIPv6 {
			return errors.New("warp.source_ipv6 requires warp.stack to be \"ipv6\"; an IPv4 stack cannot present an IPv6 egress")
		}
		w.SourceIPv6 = prefix.Masked().String()
	}
	return nil
}

// UpstreamConfig holds the base URLs of the OpenCode origins. The gateway
// picks the request shape from the base URL (models.IsV1Upstream): one ending
// in /zen is the v1 lane, any other a v2 lane.
type UpstreamConfig struct {
	// Zen is the base URL of the keyed Zen tier. Its default,
	// https://opencode.ai/zen, is the v1 lane, which expects the OpenCode 1.x
	// client request shape.
	Zen string `json:"zen"`
	// Go is the base URL of the keyed Go plan tier. Its default,
	// https://opencode.ai/zen/go, is a v2 lane, which expects the OpenCode 2.x
	// CLI shape. The WebUI also reads the plan's quota from its /v1/usage.
	Go string `json:"go"`
	// Anonymous is the v2 /inference origin of the keyless free tier. The
	// anonymous lane uses only this origin: the v1 /zen fallback is retired for
	// free models and its call site is commented out. Empty means
	// https://opencode.ai/inference.
	Anonymous string `json:"anonymous"`
}

// RetryConfig bounds the upstream retries. MaxAttempts caps the attempts of a
// retry loop: the keyed attempts of a client request outside the rotation
// path, and the catalog refresh. For the anonymous lane it is instead a floor
// for how many rotating proxies may be drawn. TimeoutSeconds is the deadline
// of the whole client request, retries included.
type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

// ModelsConfig controls model discovery and routing. RefreshSeconds is the
// catalog refresh interval, and Protocols maps a model ID to the wire protocol
// used upstream, overriding the one the catalog reports.
type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
	// FreeOnly limits discovery and routing to free models, even when Zen or
	// Go keys could serve paid ones. Free models may still fall back to keys.
	FreeOnly bool `json:"free_only"`
	// AnonymousBackoff configures a circuit breaker for the keyless lane:
	// after Threshold consecutive failed attempts, or when every exit is under
	// the free tier's IP block, the lane pauses for BaseHours, doubling on
	// every further trip up to MaxHours. An absent block leaves the lane
	// unpaused, which is the previous behavior.
	AnonymousBackoff *AnonymousBackoffConfig `json:"anonymous_backoff,omitempty"`
}

// AnonymousBackoffConfig tunes the keyless lane's circuit breaker. Zero fields
// take the defaults shown in Normalize.
type AnonymousBackoffConfig struct {
	Threshold int `json:"threshold,omitempty"`
	BaseHours int `json:"base_hours,omitempty"`
	MaxHours  int `json:"max_hours,omitempty"`
}

// Normalize fills the defaults and rejects nonsensical windows.
func (b *AnonymousBackoffConfig) Normalize() error {
	if b == nil {
		return nil
	}
	if b.Threshold == 0 {
		b.Threshold = 6
	}
	if b.BaseHours == 0 {
		b.BaseHours = 1
	}
	if b.MaxHours == 0 {
		b.MaxHours = 24
	}
	if b.Threshold < 1 {
		return errors.New("models.anonymous_backoff.threshold must be at least 1")
	}
	if b.BaseHours < 1 {
		return errors.New("models.anonymous_backoff.base_hours must be at least 1")
	}
	if b.MaxHours < b.BaseHours {
		return errors.New("models.anonymous_backoff.max_hours must not be smaller than base_hours")
	}
	return nil
}

// LoggingConfig sets the minimum log level (debug, info, warn or error) and
// RingSize, the number of recent log events kept in memory for the WebUI.
type LoggingConfig struct {
	Level    string `json:"level"`
	RingSize int    `json:"ring_size"`
	// DumpRequestBodies logs the exact bytes sent upstream (redacted, capped)
	// so retry and translation problems can be diagnosed without patching the
	// binary. Off by default: the bodies contain conversation content.
	DumpRequestBodies bool `json:"dump_request_bodies"`
}

// WebUIConfig configures the management WebUI. Password is a plaintext
// password supplied only to set one: it is hashed into PasswordHash and
// cleared before the configuration is saved. SessionTTLMinutes is the
// lifetime of a login session. Changes to Enabled and Listen take effect only
// after a restart.
type WebUIConfig struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	Password          string `json:"password,omitempty"`
	PasswordHash      string `json:"password_hash,omitempty"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

// PerformanceConfig tunes the shared upstream transports and failure
// handling: the connection pool limits (MaxConnsPerHost 0 means unlimited),
// the dial timeout, the base cooldown after an upstream key or anonymous
// proxy fails, and the per-attempt and first-event timeouts.
// AttemptTimeoutSeconds 0 leaves only the request-level timeout in place,
// FirstEventTimeoutSeconds 0 disables the first-event wait, MaxAnonymousExits
// 0 uses the built-in cap, and FreeTierBlockSeconds 0 uses the built-in
// cool-down for an exit the free tier flagged.
type PerformanceConfig struct {
	MaxIdleConns             int `json:"max_idle_conns"`
	MaxIdleConnsPerHost      int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost          int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds   int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds    int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds   int `json:"failure_cooldown_seconds"`
	AttemptTimeoutSeconds    int `json:"attempt_timeout_seconds"`
	FirstEventTimeoutSeconds int `json:"first_event_timeout_seconds"`
	// MaxAnonymousExits bounds how many distinct static proxy exits one
	// anonymous request may walk. Each exit may still try the v2 /inference
	// lane and the v1 /zen lane, so the upstream request count can be twice
	// this value; a rotating #pool entry is drawn once per retry.max_attempts
	// instead.
	MaxAnonymousExits int `json:"max_anonymous_exits"`
	// FreeTierBlockSeconds is how long an exit the free tier flagged as
	// abusive stays out of the anonymous cursor. Upstream does not publish
	// the block duration, so the default is a conservative 15 minutes.
	FreeTierBlockSeconds int `json:"free_tier_block_seconds"`
}

// AttemptTimeout returns the bound for a single upstream attempt. When
// attempt_timeout_seconds is positive the result is that setting, capped at
// requestTimeout only when requestTimeout is positive. Otherwise the result is
// requestTimeout unchanged, so a config without the setting keeps using the
// request-level retry timeout.
//
// The gateway installs the result on the shared transports as the
// response-header wait. When the setting is positive, the inference attempts
// except /systemone also arm it as a context deadline (the rotation path keeps
// only the header wait, since it has its own first-output timer), covering the
// whole attempt up to the response being released (dial, request write,
// headers and, when enabled, the first SSE event); an established stream
// keeps flowing under the request-level timeout. Without that deadline, one
// hung exit can consume the entire request budget by itself, and the attempts
// that follow are fired against an already-expired context.
func (cfg PerformanceConfig) AttemptTimeout(requestTimeout time.Duration) time.Duration {
	if cfg.AttemptTimeoutSeconds > 0 {
		attempt := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
		if requestTimeout > 0 && attempt > requestTimeout {
			return requestTimeout
		}
		return attempt
	}
	return requestTimeout
}

// Load reads the config file at path, strips its // and /* */ comments, and
// decodes it over the built-in defaults. Decoding is strict: unknown fields
// and anything after the first JSON value are errors. The result is then
// checked by Normalize.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	data, err = stripJSONComments(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		Warp:        WarpConfig{PoolSize: 2, Stack: string(warp.DefaultStack)},
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 2048, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 120, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info", RingSize: 2000},
		WebUI:       WebUIConfig{Listen: "0.0.0.0:8081", SessionTTLMinutes: 720},
		Prefer:      TierGo,
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := jsonutil.EnsureEOF(dec); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return Normalize(path, cfg)
}

// Normalize resolves external inputs and validates a Config supplied by
// either the JSON file or the authenticated management API. A relative
// proxyfile is resolved against the directory of path.
func Normalize(path string, cfg Config) (Config, error) {
	cfg.Rotation.Defaults()
	if err := cfg.Rotation.Validate(nil); err != nil {
		return Config{}, err
	}
	if err := cfg.Warp.Normalize(); err != nil {
		return Config{}, err
	}
	trimList(&cfg.ServerKeys)
	remapServerKeyMetadata(&cfg)

	trimList(&cfg.ZenKeys)
	trimList(&cfg.GoKeys)
	// A placeholder copied from the documentation is not a credential. Treating
	// it as one routes every free-model failure into a 401 from the keyed tier.
	cfg = DropPlaceholderKeys(cfg)
	cfg.ProxyFile = strings.TrimSpace(cfg.ProxyFile)
	if err := resolveProxyFiles(path, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Prefer != TierZen && cfg.Prefer != TierGo {
		return Config{}, errors.New("prefer must be \"zen\" or \"go\"")
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("listen must not be empty")
	}
	cfg.Upstream.Zen = strings.TrimSpace(cfg.Upstream.Zen)
	cfg.Upstream.Go = strings.TrimSpace(cfg.Upstream.Go)
	cfg.Upstream.Anonymous = strings.TrimSpace(cfg.Upstream.Anonymous)
	if cfg.Upstream.Anonymous == "" {
		cfg.Upstream.Anonymous = "https://opencode.ai/inference"
	}
	for name, raw := range map[string]string{"upstream.zen": cfg.Upstream.Zen, "upstream.go": cfg.Upstream.Go, "upstream.anonymous": cfg.Upstream.Anonymous} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
			return Config{}, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	if cfg.FirstEnabledServerKey() == "" {
		return Config{}, errors.New("at least one server key must remain enabled")
	}
	if !cfg.Anonymous && len(cfg.ZenKeys) == 0 && len(cfg.GoKeys) == 0 {
		return Config{}, errors.New("zen_keys or go_keys must contain at least one upstream key unless anonymous is enabled")
	}
	if cfg.Retry.MaxAttempts < 1 {
		return Config{}, errors.New("retry.max_attempts must be at least 1")
	}
	if cfg.Retry.TimeoutSeconds < 1 {
		return Config{}, errors.New("retry.timeout_seconds must be at least 1")
	}
	if cfg.Models.RefreshSeconds < 1 {
		return Config{}, errors.New("models.refresh_seconds must be at least 1")
	}
	if cfg.Performance.MaxIdleConns < 1 || cfg.Performance.MaxIdleConnsPerHost < 1 || cfg.Performance.MaxConnsPerHost < 0 || cfg.Performance.IdleConnTimeoutSeconds < 1 || cfg.Performance.ConnectTimeoutSeconds < 1 || cfg.Performance.FailureCooldownSeconds < 1 {
		return Config{}, errors.New("performance values must be positive (max_conns_per_host may be zero for unlimited)")
	}
	if cfg.Performance.AttemptTimeoutSeconds < 0 {
		return Config{}, errors.New("performance.attempt_timeout_seconds must not be negative (0 keeps the retry timeout)")
	}
	if cfg.Performance.FirstEventTimeoutSeconds < 0 || cfg.Performance.FirstEventTimeoutSeconds > 86400 {
		return Config{}, errors.New("performance.first_event_timeout_seconds must be between 0 and 86400 (0 disables the first-event timeout)")
	}
	if cfg.Performance.MaxAnonymousExits < 0 {
		return Config{}, errors.New("performance.max_anonymous_exits must not be negative (0 uses the built-in cap)")
	}
	if cfg.Performance.FreeTierBlockSeconds < 0 {
		return Config{}, errors.New("performance.free_tier_block_seconds must not be negative (0 uses the built-in cool-down)")
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return Config{}, errors.New("logging.level must be debug, info, warn, or error")
	}
	if cfg.Logging.RingSize < 100 || cfg.Logging.RingSize > 50000 {
		return Config{}, errors.New("logging.ring_size must be between 100 and 50000")
	}
	if cfg.WebUI.Password != "" && len(cfg.WebUI.Password) < 10 {
		return Config{}, errors.New("webui.password must contain at least 10 characters")
	}
	if cfg.WebUI.Enabled {
		cfg.WebUI.Listen = strings.TrimSpace(cfg.WebUI.Listen)
		cfg.WebUI.Username = strings.TrimSpace(cfg.WebUI.Username)
		if cfg.WebUI.Listen == "" {
			return Config{}, errors.New("webui.listen must not be empty when webui is enabled")
		}
		if cfg.WebUI.Username == "" {
			return Config{}, errors.New("webui.username must not be empty when webui is enabled")
		}
		if cfg.WebUI.Password == "" && cfg.WebUI.PasswordHash == "" {
			return Config{}, errors.New("webui.password is required for first-time setup")
		}
		if cfg.WebUI.SessionTTLMinutes < 5 || cfg.WebUI.SessionTTLMinutes > 10080 {
			return Config{}, errors.New("webui.session_ttl_minutes must be between 5 and 10080")
		}
	}
	for _, raw := range cfg.RuntimeProxies() {
		if raw == "direct" {
			continue
		}
		clean, _ := ParseProxyEntry(raw)
		u, err := url.Parse(clean)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", RedactURL(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	if err := cfg.Models.AnonymousBackoff.Normalize(); err != nil {
		return Config{}, err
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !wire.Valid(wire.Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	if err := normalizeReasoning(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func remapServerKeyMetadata(cfg *Config) {
	if len(cfg.ServerKeyMetadata) == 0 {
		return
	}
	for _, key := range cfg.ServerKeys {
		oldID := Fingerprint(key)[:10]
		metadata, exists := cfg.ServerKeyMetadata[oldID]
		if !exists {
			continue
		}
		newID := Fingerprint(key)
		if _, alreadyPresent := cfg.ServerKeyMetadata[newID]; !alreadyPresent {
			cfg.ServerKeyMetadata[newID] = metadata
		}
		delete(cfg.ServerKeyMetadata, oldID)
	}
}

// normalizeReasoning validates and canonicalizes the forced thinking level.
//
// Levels are stored lowercased so the hot path can compare them without
// allocating. An empty value means "not forced", which is the default and keeps
// the feature absent for existing configurations.
func normalizeReasoning(cfg *Config) error {
	cfg.Reasoning.Effort = strings.ToLower(strings.TrimSpace(cfg.Reasoning.Effort))
	if err := validateEffort("reasoning.effort", cfg.Reasoning.Effort); err != nil {
		return err
	}
	if cfg.Reasoning.EffortByModel == nil {
		return nil
	}
	// An empty map is the same as an absent one, and writing nil back keeps a
	// saved configuration free of a field the user never set.
	if len(cfg.Reasoning.EffortByModel) == 0 {
		cfg.Reasoning.EffortByModel = nil
		return nil
	}
	normalized := make(map[string]string, len(cfg.Reasoning.EffortByModel))
	for model, effort := range cfg.Reasoning.EffortByModel {
		key := strings.TrimSpace(model)
		if key == "" {
			return errors.New("reasoning.effort_by_model must not contain an empty model ID")
		}
		level := strings.ToLower(strings.TrimSpace(effort))
		if err := validateEffort(fmt.Sprintf("reasoning.effort_by_model[%q]", key), level); err != nil {
			return err
		}
		normalized[key] = level
	}
	cfg.Reasoning.EffortByModel = normalized
	return nil
}

// validateEffort accepts the levels the OpenAI and Anthropic surfaces define
// (minimal through max), the gateway controls "auto" and "none", and an empty
// string, which means "not forced".
func validateEffort(name, value string) error {
	switch value {
	case "", "auto", "minimal", "low", "medium", "high", "xhigh", "max", "none":
		return nil
	default:
		return fmt.Errorf("%s must be one of auto, minimal, low, medium, high, xhigh, max, none, or empty to disable the override", name)
	}
}

// ForcedEffort returns the level the operator forced for a model, or an empty
// string when none applies. A per-model entry, even an empty one, takes
// precedence over the default. A stored level is already lowercase.
func (cfg Config) ForcedEffort(model string) string {
	if effort, ok := cfg.Reasoning.EffortByModel[model]; ok {
		return effort
	}
	return cfg.Reasoning.Effort
}

// RuntimeProxies returns the resolved proxy list, including proxyfile entries,
// or ["direct"] when there are none. Config.Proxies intentionally remains the
// list stored in config.json so a save never duplicates values loaded from
// proxyfile.
func (cfg Config) RuntimeProxies() []string {
	if len(cfg.effectiveProxies) > 0 {
		return cfg.effectiveProxies
	}
	if len(cfg.Proxies) > 0 {
		return cfg.Proxies
	}
	return []string{"direct"}
}

// PasswordForSave clears the resolved proxy list (the proxies plus proxyfile
// entries that RuntimeProxies returns), leaving only Config.Proxies; despite
// its name it does not touch the WebUI password. SaveAtomic calls it on its
// own copy before encoding. The field is unexported and so never encoded
// anyway, which makes the call a guard rather than a requirement.
func (cfg *Config) PasswordForSave() {
	cfg.effectiveProxies = nil
}

// Clone returns a copy of cfg whose slices and maps, including the rotation
// orders and the resolved proxy list, no longer share storage with cfg.
func Clone(cfg Config) Config {
	cfg.Rotation.SOTA.Order = append([]string(nil), cfg.Rotation.SOTA.Order...)
	cfg.Rotation.Sweet.Order = append([]string(nil), cfg.Rotation.Sweet.Order...)
	cfg.ServerKeys = append([]string(nil), cfg.ServerKeys...)
	metadata := make(map[string]ServerKeyMetadata, len(cfg.ServerKeyMetadata))
	for id, item := range cfg.ServerKeyMetadata {
		metadata[id] = item
	}
	cfg.ServerKeyMetadata = metadata
	cfg.ZenKeys = append([]string(nil), cfg.ZenKeys...)
	cfg.GoKeys = append([]string(nil), cfg.GoKeys...)
	cfg.Proxies = append([]string(nil), cfg.Proxies...)
	cfg.effectiveProxies = append([]string(nil), cfg.effectiveProxies...)
	if cfg.Models.Protocols != nil {
		protocols := make(map[string]string, len(cfg.Models.Protocols))
		for key, value := range cfg.Models.Protocols {
			protocols[key] = value
		}
		cfg.Models.Protocols = protocols
	}
	if cfg.Reasoning.EffortByModel != nil {
		efforts := make(map[string]string, len(cfg.Reasoning.EffortByModel))
		for model, effort := range cfg.Reasoning.EffortByModel {
			efforts[model] = effort
		}
		cfg.Reasoning.EffortByModel = efforts
	}
	return cfg
}
