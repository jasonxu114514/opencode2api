package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	updatedAt       time.Time
	prefer          Tier
	metadata        *modelMetadataStore
}

type modelCatalogSnapshot struct {
	Zen       int       `json:"zen"`
	Go        int       `json:"go"`
	Total     int       `json:"total"`
	Exposed   int       `json:"exposed"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
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
	}
}

func (c *modelCatalog) Replace(zen, goModels []string) {
	c.ReplaceWithCapabilities(zen, goModels, nil, nil)
}

func (c *modelCatalog) ReplaceWithCapabilities(zen, goModels []string, native map[Tier]map[string]Protocol, unsupported map[Tier]map[string]bool) {
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
	c.updatedAt = time.Now()
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
	updatedAt := source.updatedAt
	source.mu.RUnlock()
	c.mu.Lock()
	c.zen, c.goModels, c.nativeProtocols, c.unsupported, c.updatedAt = zen, goModels, native, unsupported, updatedAt
	c.mu.Unlock()
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
	return modelCatalogSnapshot{
		Zen:       len(c.zen),
		Go:        len(c.goModels),
		Total:     len(seen),
		Exposed:   exposed,
		UpdatedAt: c.updatedAt,
	}
}

func (c *modelCatalog) Supported(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supportedLocked(model)
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

type protocolCapabilities struct {
	Protocols   map[Tier]map[string]Protocol
	Unsupported map[Tier]map[string]bool
}

type capabilityProvider struct {
	ID     string                     `json:"id"`
	API    string                     `json:"api"`
	NPM    string                     `json:"npm"`
	Models map[string]capabilityModel `json:"models"`
}

type capabilityModel struct {
	ID       string                   `json:"id"`
	Provider *capabilityModelProvider `json:"provider"`
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
