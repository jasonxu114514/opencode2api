// Package config loads, validates, persists, and redacts service configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
)

type Config struct {
	Rotation          RotationConfig               `json:"rotation"`
	Listen            string                       `json:"listen"`
	ServerKeys        []string                     `json:"server_keys"`
	ServerKeyMetadata map[string]ServerKeyMetadata `json:"server_key_metadata,omitempty"`
	ZenKeys           []string                     `json:"zen_keys"`
	GoKeys            []string                     `json:"go_keys"`
	Anonymous         bool                         `json:"anonymous"`
	Proxies           []string                     `json:"proxies"`
	ProxyFile         string                       `json:"proxyfile"`
	Upstream          UpstreamConfig               `json:"upstream"`
	Retry             RetryConfig                  `json:"retry"`
	Models            ModelsConfig                 `json:"models"`
	Performance       PerformanceConfig            `json:"performance"`
	Logging           LoggingConfig                `json:"logging"`
	WebUI             WebUIConfig                  `json:"webui"`
	Prefer            Tier                         `json:"prefer"`

	effectiveProxies []string
}

type UpstreamConfig struct {
	Zen string `json:"zen"`
	Go  string `json:"go"`
}

type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
}

type LoggingConfig struct {
	Level    string `json:"level"`
	RingSize int    `json:"ring_size"`
	// DumpRequestBodies logs the exact bytes sent upstream (redacted, capped)
	// so retry and translation problems can be diagnosed without patching the
	// binary. Off by default: the bodies contain conversation content.
	DumpRequestBodies bool `json:"dump_request_bodies"`
}

type WebUIConfig struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	Password          string `json:"password,omitempty"`
	PasswordHash      string `json:"password_hash,omitempty"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

type PerformanceConfig struct {
	MaxIdleConns           int `json:"max_idle_conns"`
	MaxIdleConnsPerHost    int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost        int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds  int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
	AttemptTimeoutSeconds  int `json:"attempt_timeout_seconds"`
}

// AttemptTimeout bounds how long a single upstream attempt may wait for
// response headers before it is abandoned and the next node is tried. Values
// <= 0 keep the historical behavior of using the request-level retry timeout,
// so existing configs are unaffected. The result never exceeds requestTimeout,
// and only the header wait is bounded: an established stream keeps flowing
// under the request-level timeout.
//
// The bound is installed on the shared transports, so it covers every attempt
// in both the anonymous and the authenticated loops. Without it, one hung exit
// can consume the entire request budget by itself, and the attempts that follow
// are fired against an already-expired context.
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
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go"},
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
// either the JSON file or the authenticated management API.
func Normalize(path string, cfg Config) (Config, error) {
	cfg.Rotation.Defaults()
	if err := cfg.Rotation.Validate(nil); err != nil {
		return Config{}, err
	}
	trimList(&cfg.ServerKeys)
	trimList(&cfg.ZenKeys)
	trimList(&cfg.GoKeys)
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
	for name, raw := range map[string]string{"upstream.zen": cfg.Upstream.Zen, "upstream.go": cfg.Upstream.Go} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
			return Config{}, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	if cfg.FirstEnabledServerKey() == "" {
		return Config{}, errors.New("至少保留一个启用的 API Key")
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
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", RedactURL(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !wire.Valid(wire.Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	return cfg, nil
}

// RuntimeProxies returns the resolved proxy list, including proxyfile entries.
// Config.Proxies intentionally remains the list stored in config.json so a
// save never duplicates values loaded from proxyfile.
func (cfg Config) RuntimeProxies() []string {
	if len(cfg.effectiveProxies) > 0 {
		return cfg.effectiveProxies
	}
	if len(cfg.Proxies) > 0 {
		return cfg.Proxies
	}
	return []string{"direct"}
}

// PasswordForSave ensures resolved-only data is excluded. The method is kept
// separate to make accidental persistence of effective proxy values obvious.
func (cfg *Config) PasswordForSave() {
	cfg.effectiveProxies = nil
}

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
	return cfg
}
