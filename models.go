package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolAnthropic Protocol = "anthropic"
)

const (
	openCodeCapabilitiesURL = "https://models.opencode.ai/api.json"
	openCodeZenDocsURL      = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
	openCodeGoDocsURL       = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/go.mdx"
)

var protocolDocEndpointPattern = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages)`")

func validProtocol(p Protocol) bool {
	return p == ProtocolChat || p == ProtocolResponses || p == ProtocolAnthropic
}

type Tier string

const (
	TierZen Tier = "zen"
	TierGo  Tier = "go"
)

type modelRoute struct {
	ID       string
	Tier     Tier
	Protocol Protocol
	// Protocols is the native protocol for each possible upstream tier. Zen
	// and Go intentionally do not share one global protocol: OpenCode's
	// catalog currently exposes, for example, MiniMax through Chat on Zen and
	// Messages on Go. The request must therefore be re-encoded when a retry
	// crosses tiers.
	Protocols map[Tier]Protocol
	Anonymous bool
	// KeyTiers is the ordered authenticated fallback plan. Anonymous requests
	// always start on Zen, then enter this list when the public credential does
	// not succeed.
	KeyTiers []Tier
}

type ModelRouteDiagnostic struct {
	Model                string            `json:"model"`
	RequestedProtocol    Protocol          `json:"requested_protocol,omitempty"`
	NativeProtocol       Protocol          `json:"native_protocol"`
	NativeProtocols      map[Tier]Protocol `json:"native_protocols,omitempty"`
	ProtocolSource       string            `json:"protocol_source"`
	AvailableZen         bool              `json:"available_zen"`
	AvailableGo          bool              `json:"available_go"`
	Tier                 Tier              `json:"tier,omitempty"`
	Anonymous            bool              `json:"anonymous"`
	KeyID                string            `json:"key_id,omitempty"`
	Channel              string            `json:"channel,omitempty"`
	Attempts             int               `json:"attempts,omitempty"`
	KeyTiers             []Tier            `json:"key_tiers,omitempty"`
	AnonymousEligibility AnonymousDecision `json:"anonymous_eligibility"`
	RouteError           string            `json:"route_error,omitempty"`
}

type modelCatalog struct {
	mu        sync.RWMutex
	zen       map[string]bool
	goModels  map[string]bool
	protocols map[string]Protocol
	// nativeProtocols is populated from OpenCode's public model capability
	// catalog. protocols remains the user-configured override map.
	nativeProtocols map[Tier]map[string]Protocol
	unsupported     map[Tier]map[string]bool
	modelMeta       map[Tier]map[string]ModelMetadata
	updatedAt       time.Time
	prefer          Tier
	metadata        *modelMetadataStore
	cachePath       string
	cacheSource     string
	stale           bool
	refreshAfter    time.Duration
}

type modelCatalogSnapshot struct {
	Zen         int       `json:"zen"`
	Go          int       `json:"go"`
	Total       int       `json:"total"`
	Exposed     int       `json:"exposed"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	CacheSource string    `json:"cache_source,omitempty"`
	Stale       bool      `json:"stale"`
}

const modelCatalogCacheSchemaVersion = 3

var modelCatalogCacheWriteMu sync.Mutex

type modelCatalogCache struct {
	SchemaVersion   int                               `json:"schema_version"`
	UpdatedAt       time.Time                         `json:"updated_at"`
	Zen             []string                          `json:"zen"`
	Go              []string                          `json:"go"`
	NativeProtocols map[Tier]map[string]Protocol      `json:"native_protocols"`
	Unsupported     map[Tier]map[string]bool          `json:"unsupported"`
	Metadata        map[Tier]map[string]ModelMetadata `json:"metadata,omitempty"`
}

func newModelCatalog(prefer Tier, overrides map[string]string) *modelCatalog {
	protocols := make(map[string]Protocol, len(overrides))
	for model, protocol := range overrides {
		protocols[model] = Protocol(protocol)
	}
	return &modelCatalog{
		zen: map[string]bool{}, goModels: map[string]bool{}, protocols: protocols,
		nativeProtocols: map[Tier]map[string]Protocol{TierZen: {}, TierGo: {}},
		unsupported:     map[Tier]map[string]bool{TierZen: {}, TierGo: {}}, prefer: prefer,
		cacheSource: "none",
	}
}

func (c *modelCatalog) SetCachePath(path string) {
	c.mu.Lock()
	c.cachePath = path
	c.mu.Unlock()
}

func (c *modelCatalog) SetRefreshInterval(interval time.Duration) {
	c.mu.Lock()
	c.refreshAfter = interval
	c.mu.Unlock()
}

func (c *modelCatalog) Replace(zen, goModels []string) {
	c.ReplaceWithCapabilities(zen, goModels, nil, nil, nil)
}

func (c *modelCatalog) ReplaceWithCapabilities(zen, goModels []string, native map[Tier]map[string]Protocol, unsupported map[Tier]map[string]bool, metadata map[Tier]map[string]ModelMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if zen != nil {
		c.zen = toSet(zen)
	}
	if goModels != nil {
		c.goModels = toSet(goModels)
	}
	if native != nil {
		for _, tier := range []Tier{TierZen, TierGo} {
			if protocols, ok := native[tier]; ok {
				c.nativeProtocols[tier] = cloneProtocols(protocols)
			}
		}
	}
	if unsupported != nil {
		for _, tier := range []Tier{TierZen, TierGo} {
			if models, ok := unsupported[tier]; ok {
				c.unsupported[tier] = cloneBools(models)
			}
		}
	}
	if metadata != nil {
		c.modelMeta = cloneModelMeta(metadata)
	}
	c.updatedAt = time.Now().UTC()
	c.cacheSource = "live"
	c.stale = false
}

func (c *modelCatalog) CopyState(source *modelCatalog) {
	if source == nil {
		return
	}
	source.mu.RLock()
	zen := make(map[string]bool, len(source.zen))
	goModels := make(map[string]bool, len(source.goModels))
	for model, available := range source.zen {
		zen[model] = available
	}
	for model, available := range source.goModels {
		goModels[model] = available
	}
	native := map[Tier]map[string]Protocol{TierZen: {}, TierGo: {}}
	unsupported := map[Tier]map[string]bool{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		for model, protocol := range source.nativeProtocols[tier] {
			native[tier][model] = protocol
		}
		for model, value := range source.unsupported[tier] {
			unsupported[tier][model] = value
		}
	}
	meta := cloneModelMeta(source.modelMeta)
	updatedAt := source.updatedAt
	cacheSource := source.cacheSource
	stale := source.stale
	source.mu.RUnlock()
	c.mu.Lock()
	c.zen, c.goModels, c.nativeProtocols, c.unsupported, c.updatedAt = zen, goModels, native, unsupported, updatedAt
	c.modelMeta = meta
	c.cacheSource, c.stale = cacheSource, stale
	c.mu.Unlock()
}

// LoadCache installs a validated disk snapshot into a catalog. It deliberately
// changes only discovered state; configured protocol overrides and routing
// preferences stay owned by the current Config.
func (c *modelCatalog) LoadCache(path string) error {
	cache, err := loadModelCatalogCache(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.zen = toSet(cache.Zen)
	c.goModels = toSet(cache.Go)
	c.nativeProtocols = cloneTierProtocols(cache.NativeProtocols)
	c.unsupported = cloneTierBools(cache.Unsupported)
	c.modelMeta = cloneModelMeta(cache.Metadata)
	c.updatedAt = cache.UpdatedAt.UTC()
	c.cacheSource = "disk"
	c.stale = true
	if c.cachePath == "" {
		c.cachePath = path
	}
	c.mu.Unlock()
	return nil
}

// SaveCache snapshots only public model capability data. Credentials and
// proxy configuration are not part of modelCatalog and can never enter this
// file.
func (c *modelCatalog) SaveCache() error {
	c.mu.RLock()
	path := c.cachePath
	cache := modelCatalogCache{
		SchemaVersion:   modelCatalogCacheSchemaVersion,
		UpdatedAt:       c.updatedAt.UTC(),
		Zen:             sortedSetKeys(c.zen),
		Go:              sortedSetKeys(c.goModels),
		NativeProtocols: cloneTierProtocols(c.nativeProtocols),
		Unsupported:     cloneTierBools(c.unsupported),
		Metadata:        cloneModelMeta(c.modelMeta),
	}
	c.mu.RUnlock()
	if path == "" {
		return nil
	}
	return saveModelCatalogCache(path, cache)
}

func (c *modelCatalog) Route(model string, hasZenKeys, hasGoKeys, hasAnonymous bool) (modelRoute, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keyTiers := c.keyTierOrderLocked(model, hasZenKeys, hasGoKeys)
	// OpenCode's public credential is a Zen-only lane. Every free model starts
	// there, even if the current catalog only advertises it on Go: an upstream
	// rejection will move the request into the authenticated fallback plan.
	decision := c.anonymousDecision(model)
	if hasAnonymous && decision.Allowed && (c.protocols[model] != "" || !c.unsupported[TierZen][model]) &&
		(len(c.zen) == 0 && len(c.goModels) == 0 || c.zen[model] || c.goModels[model]) {
		protocols := c.protocolsForLocked(model, keyTiers, true)
		return modelRoute{ID: model, Tier: TierZen, Protocol: protocols[TierZen], Protocols: protocols, Anonymous: true, KeyTiers: keyTiers}, nil
	}
	if len(keyTiers) > 0 {
		protocols := c.protocolsForLocked(model, keyTiers, false)
		return modelRoute{ID: model, Tier: keyTiers[0], Protocol: protocols[keyTiers[0]], Protocols: protocols, KeyTiers: keyTiers}, nil
	}
	return modelRoute{}, fmt.Errorf("model %q is not available in the configured Zen or Go pools", model)
}

func (r modelRoute) ProtocolFor(tier Tier) Protocol {
	if protocol := r.Protocols[tier]; protocol != "" {
		return protocol
	}
	return r.Protocol
}

func (c *modelCatalog) protocolsForLocked(model string, keyTiers []Tier, includeZen bool) map[Tier]Protocol {
	protocols := make(map[Tier]Protocol, len(keyTiers)+1)
	if includeZen {
		protocols[TierZen] = c.protocolForLocked(model, TierZen)
	}
	for _, tier := range keyTiers {
		protocols[tier] = c.protocolForLocked(model, tier)
	}
	return protocols
}

func (c *modelCatalog) protocolForLocked(model string, tier Tier) Protocol {
	if protocol := c.protocols[model]; protocol != "" {
		return protocol
	}
	if protocol := c.nativeProtocols[tier][model]; protocol != "" {
		return protocol
	}
	// The OpenCode capability catalog is authoritative when available. Chat is
	// the only safe protocol-neutral fallback for an ID that has just appeared
	// in /v1/models but is not present in the capability snapshot yet.
	return ProtocolChat
}

// keyTierOrderLocked builds an authenticated route in prefer order. A tier is
// included only when it has a key and advertises the model. Before the first
// successful catalog refresh, configured key pools remain usable so temporary
// discovery failures do not take the gateway offline.
func (c *modelCatalog) keyTierOrderLocked(model string, hasZenKeys, hasGoKeys bool) []Tier {
	catalogPending := len(c.zen) == 0 && len(c.goModels) == 0
	available := func(tier Tier) bool {
		switch tier {
		case TierZen:
			return hasZenKeys && (catalogPending || c.zen[model]) && c.tierSupportedLocked(model, TierZen)
		case TierGo:
			return hasGoKeys && (catalogPending || c.goModels[model]) && c.tierSupportedLocked(model, TierGo)
		default:
			return false
		}
	}
	order := []Tier{TierZen, TierGo}
	if c.prefer == TierGo {
		order[0], order[1] = order[1], order[0]
	}
	result := make([]Tier, 0, len(order))
	for _, tier := range order {
		if available(tier) {
			result = append(result, tier)
		}
	}
	return result
}

func (c *modelCatalog) anonymousDecision(model string) AnonymousDecision {
	if c.metadata != nil {
		return c.metadata.Decide(model)
	}
	return AnonymousDecision{Allowed: isFreeModel(model), Source: "name_fallback_metadata_pending"}
}

func (c *modelCatalog) Diagnostic(model string, requested Protocol, hasZenKeys, hasGoKeys, hasAnonymous bool) ModelRouteDiagnostic {
	c.mu.RLock()
	configured, explicit := c.protocols[model]
	zen, goModel := c.zen[model], c.goModels[model]
	nativeProtocols := map[Tier]Protocol{
		TierZen: c.protocolForLocked(model, TierZen),
		TierGo:  c.protocolForLocked(model, TierGo),
	}
	_, zenKnown := c.nativeProtocols[TierZen][model]
	_, goKnown := c.nativeProtocols[TierGo][model]
	c.mu.RUnlock()
	source := "configured"
	if !explicit {
		source = "default"
		if zenKnown || goKnown {
			source = "upstream"
		}
	}
	protocol := configured
	if protocol == "" {
		// Route() below selects the preferred available tier. This is only the
		// fallback shown when no route can currently be built.
		protocol = nativeProtocols[TierZen]
		if c.prefer == TierGo {
			protocol = nativeProtocols[TierGo]
		}
	}
	diagnostic := ModelRouteDiagnostic{
		Model: model, RequestedProtocol: requested, NativeProtocol: protocol, NativeProtocols: nativeProtocols, ProtocolSource: source,
		AvailableZen: zen, AvailableGo: goModel, AnonymousEligibility: c.anonymousDecision(model),
	}
	route, err := c.Route(model, hasZenKeys, hasGoKeys, hasAnonymous)
	if err != nil {
		diagnostic.RouteError = err.Error()
		return diagnostic
	}
	diagnostic.NativeProtocol = route.Protocol
	diagnostic.NativeProtocols = route.Protocols
	diagnostic.Tier, diagnostic.Anonymous = route.Tier, route.Anonymous
	diagnostic.KeyTiers = append([]Tier(nil), route.KeyTiers...)
	return diagnostic
}

func isFreeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "free")
}

func (c *modelCatalog) List() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]bool, len(c.zen)+len(c.goModels))
	for model := range c.zen {
		seen[model] = true
	}
	for model := range c.goModels {
		seen[model] = true
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		if c.supportedLocked(model) {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func (c *modelCatalog) Snapshot() modelCatalogSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := make(map[string]bool, len(c.zen)+len(c.goModels))
	for model := range c.zen {
		seen[model] = true
	}
	for model := range c.goModels {
		seen[model] = true
	}
	exposed := 0
	for model := range seen {
		if c.supportedLocked(model) {
			exposed++
		}
	}
	stale := c.stale
	if !c.updatedAt.IsZero() && c.refreshAfter > 0 {
		stale = stale || time.Since(c.updatedAt) > max(2*c.refreshAfter, time.Minute)
	}
	return modelCatalogSnapshot{
		Zen: len(c.zen), Go: len(c.goModels), Total: len(seen), Exposed: exposed,
		UpdatedAt: c.updatedAt, CacheSource: c.cacheSource, Stale: stale,
	}
}

func (c *modelCatalog) Supported(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supportedLocked(model)
}

// MetadataForTier returns the rich per-model metadata (context window,
// reasoning, tool call, modalities) captured from the opencode catalog for
// the tier that will actually serve the request. Same-named models can carry
// different limits per tier, so callers must pass route.Tier — never a
// tier-blind lookup. Anonymous routes always resolve to TierZen, which keeps
// the keyless path on Zen metadata. The zero value is returned for models
// the catalog does not describe (or before the first capability refresh).
func (c *modelCatalog) MetadataForTier(model string, tier Tier) ModelMetadata {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.modelMeta[tier][model]
}

func (c *modelCatalog) supportedLocked(model string) bool {
	if len(c.zen) == 0 && len(c.goModels) == 0 {
		return true
	}
	if c.zen[model] && c.tierSupportedLocked(model, TierZen) {
		return true
	}
	if c.goModels[model] && c.tierSupportedLocked(model, TierGo) {
		return true
	}
	return false
}

func (c *modelCatalog) tierSupportedLocked(model string, tier Tier) bool {
	if c.protocols[model] != "" {
		return true
	}
	if c.unsupported[tier][model] {
		return false
	}
	if c.nativeProtocols[tier][model] != "" {
		return true
	}
	// A pending catalog has no upstream capability snapshot to contradict a
	// configured key, so retain the pre-refresh compatibility behavior.
	return len(c.zen) == 0 && len(c.goModels) == 0
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func cloneProtocols(source map[string]Protocol) map[string]Protocol {
	result := make(map[string]Protocol, len(source))
	for model, protocol := range source {
		result[model] = protocol
	}
	return result
}

func cloneBools(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for model, value := range source {
		result[model] = value
	}
	return result
}

func cloneModelMeta(source map[Tier]map[string]ModelMetadata) map[Tier]map[string]ModelMetadata {
	result := map[Tier]map[string]ModelMetadata{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		for id, md := range source[tier] {
			result[tier][id] = md
		}
	}
	return result
}

func sortedSetKeys(source map[string]bool) []string {
	result := make([]string, 0, len(source))
	for model, available := range source {
		if available {
			result = append(result, model)
		}
	}
	sort.Strings(result)
	return result
}

func cloneTierProtocols(source map[Tier]map[string]Protocol) map[Tier]map[string]Protocol {
	result := map[Tier]map[string]Protocol{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		if protocols, ok := source[tier]; ok {
			result[tier] = cloneProtocols(protocols)
		}
	}
	return result
}

func cloneTierBools(source map[Tier]map[string]bool) map[Tier]map[string]bool {
	result := map[Tier]map[string]bool{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		if models, ok := source[tier]; ok {
			result[tier] = cloneBools(models)
		}
	}
	return result
}

func modelCatalogCachePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return configPath + ".models.catalog.json"
}

func loadModelCatalogCache(path string) (modelCatalogCache, error) {
	if path == "" {
		return modelCatalogCache{}, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return modelCatalogCache{}, err
	}
	defer file.Close()
	var cache modelCatalogCache
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	if err := decoder.Decode(&cache); err != nil {
		return modelCatalogCache{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return modelCatalogCache{}, errors.New("model catalog cache contains multiple JSON values")
		}
		return modelCatalogCache{}, err
	}
	if cache.SchemaVersion != modelCatalogCacheSchemaVersion {
		return modelCatalogCache{}, fmt.Errorf("unsupported model catalog cache schema version %d", cache.SchemaVersion)
	}
	if cache.UpdatedAt.IsZero() {
		return modelCatalogCache{}, errors.New("model catalog cache is missing updated_at")
	}
	cache.Zen = normalizeModelIDs(cache.Zen)
	cache.Go = normalizeModelIDs(cache.Go)
	if len(cache.Zen) == 0 && len(cache.Go) == 0 {
		return modelCatalogCache{}, errors.New("model catalog cache is empty")
	}
	if err := validateCatalogCapabilities(cache.NativeProtocols, cache.Unsupported); err != nil {
		return modelCatalogCache{}, err
	}
	cache.NativeProtocols = cloneTierProtocols(cache.NativeProtocols)
	cache.Unsupported = cloneTierBools(cache.Unsupported)
	cache.UpdatedAt = cache.UpdatedAt.UTC()
	return cache, nil
}

func normalizeModelIDs(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

func validateCatalogCapabilities(native map[Tier]map[string]Protocol, unsupported map[Tier]map[string]bool) error {
	for tier, protocols := range native {
		if tier != TierZen && tier != TierGo {
			return fmt.Errorf("model catalog cache contains unknown tier %q", tier)
		}
		for model, protocol := range protocols {
			if model == "" || !validProtocol(protocol) {
				return fmt.Errorf("model catalog cache contains invalid protocol for %q", model)
			}
		}
	}
	for tier, models := range unsupported {
		if tier != TierZen && tier != TierGo {
			return fmt.Errorf("model catalog cache contains unknown tier %q", tier)
		}
		for model := range models {
			if strings.TrimSpace(model) == "" {
				return errors.New("model catalog cache contains an empty unsupported model")
			}
		}
	}
	return nil
}

func saveModelCatalogCache(path string, cache modelCatalogCache) error {
	if path == "" {
		return nil
	}
	modelCatalogCacheWriteMu.Lock()
	defer modelCatalogCacheWriteMu.Unlock()
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".models-catalog-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		backup := path + ".replace"
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			return err
		}
		hadOld := false
		if _, statErr := os.Stat(path); statErr == nil {
			if err := os.Rename(path, backup); err != nil {
				return err
			}
			hadOld = true
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if err := os.Rename(tempPath, path); err != nil {
			if hadOld {
				_ = os.Rename(backup, path)
			}
			return err
		}
		if hadOld {
			_ = os.Remove(backup)
		}
		return nil
	}
	return os.Rename(tempPath, path)
}

type protocolCapabilities struct {
	Protocols   map[Tier]map[string]Protocol
	Unsupported map[Tier]map[string]bool
	Metadata    map[Tier]map[string]ModelMetadata
}

type capabilityProvider struct {
	ID     string                     `json:"id"`
	API    string                     `json:"api"`
	NPM    string                     `json:"npm"`
	Models map[string]capabilityModel `json:"models"`
}

type capabilityModel struct {
	ID               string                     `json:"id"`
	Provider         *capabilityModelProvider   `json:"provider"`
	Limit            *capabilityModelLimit      `json:"limit"`
	Reasoning        bool                       `json:"reasoning"`
	ToolCall         bool                       `json:"tool_call"`
	StructuredOutput bool                       `json:"structured_output"`
	Modalities       *capabilityModelModalities `json:"modalities"`
}

type capabilityModelModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type capabilityModelLimit struct {
	Context int `json:"context"`
	Input   int `json:"input"`
	Output  int `json:"output"`
}

// ModelMetadata carries the per-model capability fields surfaced through
// /v1/models so consumers (harnesses like Pi or jcode) get real context
// windows and feature flags from the catalog instead of guessing. It is
// purely additive: routing does not depend on any of these fields.
type ModelMetadata struct {
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxInput         int      `json:"max_input,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Reasoning        bool     `json:"reasoning,omitempty"`
	ToolCall         bool     `json:"tool_call,omitempty"`
	StructuredOutput bool     `json:"structured_output,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

func (m *capabilityModel) metadata() ModelMetadata {
	md := ModelMetadata{
		Reasoning:        m.Reasoning,
		ToolCall:         m.ToolCall,
		StructuredOutput: m.StructuredOutput,
	}
	if m.Modalities != nil {
		md.InputModalities = m.Modalities.Input
		md.OutputModalities = m.Modalities.Output
	}
	if m.Limit != nil {
		md.ContextWindow = m.Limit.Context
		md.MaxInput = m.Limit.Input
		md.MaxOutput = m.Limit.Output
	}
	return md
}

type capabilityModelProvider struct {
	NPM string `json:"npm"`
}

// fetchProtocolCapabilities reads OpenCode's machine-readable provider
// catalog. Unlike /v1/models, this source includes the SDK selected for each
// model, which is the upstream's protocol declaration. No model IDs are kept
// in this project.
func fetchProtocolCapabilities(ctx context.Context, client *http.Client, endpoint string) (protocolCapabilities, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return protocolCapabilities{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", opencodeUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return protocolCapabilities{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return protocolCapabilities{}, fmt.Errorf("OpenCode capability endpoint returned HTTP %d", resp.StatusCode)
	}
	var providers map[string]capabilityProvider
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(&providers); err != nil {
		return protocolCapabilities{}, err
	}
	result := protocolCapabilities{
		Protocols:   map[Tier]map[string]Protocol{TierZen: {}, TierGo: {}},
		Unsupported: map[Tier]map[string]bool{TierZen: {}, TierGo: {}},
		Metadata:    map[Tier]map[string]ModelMetadata{TierZen: {}, TierGo: {}},
	}
	for providerID, provider := range providers {
		tier, ok := capabilityTier(providerID, provider.API)
		if !ok {
			continue
		}
		for modelID, model := range provider.Models {
			if model.ID != "" {
				modelID = model.ID
			}
			npm := provider.NPM
			if model.Provider != nil && model.Provider.NPM != "" {
				npm = model.Provider.NPM
			}
			if protocol, ok := protocolForSDK(npm); ok {
				result.Protocols[tier][modelID] = protocol
			} else {
				result.Unsupported[tier][modelID] = true
			}
			result.Metadata[tier][modelID] = model.metadata()
		}
	}
	// The machine catalog is the primary source. The upstream endpoint tables
	// are a supplemental source for models whose provider inherits a default SDK
	// but whose published endpoint is more specific (for example a Messages
	// route). This remains data-driven: no model IDs are embedded here.
	for _, doc := range []struct {
		tier Tier
		url  string
	}{
		{TierZen, openCodeZenDocsURL},
		{TierGo, openCodeGoDocsURL},
	} {
		protocols, err := fetchProtocolDocs(ctx, client, doc.url)
		if err != nil {
			continue
		}
		for modelID, protocol := range protocols {
			result.Protocols[doc.tier][modelID] = protocol
			delete(result.Unsupported[doc.tier], modelID)
		}
	}
	if len(result.Protocols[TierZen]) == 0 && len(result.Protocols[TierGo]) == 0 && len(result.Unsupported[TierZen]) == 0 && len(result.Unsupported[TierGo]) == 0 {
		return protocolCapabilities{}, errors.New("OpenCode capability endpoint returned no Zen or Go models")
	}
	return result, nil
}

func fetchProtocolDocs(ctx context.Context, client *http.Client, endpoint string) (map[string]Protocol, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	req.Header.Set("User-Agent", opencodeUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OpenCode endpoint documentation returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	result := make(map[string]Protocol)
	for _, line := range strings.Split(string(body), "\n") {
		match := protocolDocEndpointPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		modelID := strings.TrimSpace(match[1])
		if modelID == "" || strings.ContainsAny(modelID, " `|") {
			continue
		}
		var protocol Protocol
		switch match[2] {
		case "chat/completions":
			protocol = ProtocolChat
		case "responses":
			protocol = ProtocolResponses
		case "messages":
			protocol = ProtocolAnthropic
		}
		if protocol != "" {
			result[modelID] = protocol
		}
	}
	if len(result) == 0 {
		return nil, errors.New("OpenCode endpoint documentation returned no protocol rows")
	}
	return result, nil
}

func capabilityTier(providerID, api string) (Tier, bool) {
	value := strings.ToLower(strings.TrimSpace(providerID + " " + api))
	if strings.Contains(value, "opencode-go") || strings.Contains(value, "/go/") {
		return TierGo, true
	}
	if strings.Contains(value, "opencode") || strings.Contains(value, "/zen/") {
		return TierZen, true
	}
	return "", false
}

func protocolForSDK(npm string) (Protocol, bool) {
	value := strings.ToLower(strings.TrimSpace(npm))
	switch {
	case strings.Contains(value, "anthropic"):
		return ProtocolAnthropic, true
	case value == "@ai-sdk/openai" || strings.HasSuffix(value, "/openai"):
		return ProtocolResponses, true
	case strings.Contains(value, "openai-compatible"):
		return ProtocolChat, true
	default:
		return "", false
	}
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func fetchModels(ctx context.Context, client *http.Client, baseURL, key string) ([]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", opencodeUserAgent())
	req.Header.Set("x-opencode-client", "cli")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload modelsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if err := dec.Decode(&payload); err != nil {
		return nil, resp.StatusCode, err
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	if len(models) == 0 {
		return nil, resp.StatusCode, errors.New("models endpoint returned an empty list")
	}
	return models, resp.StatusCode, nil
}
