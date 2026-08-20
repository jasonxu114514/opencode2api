package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func validProtocol(p Protocol) bool {
	return p == ProtocolChat || p == ProtocolResponses || p == ProtocolAnthropic
}

type Tier string

const (
	TierZen Tier = "zen"
	TierGo  Tier = "go"
)

type modelRoute struct {
	ID        string
	Tier      Tier
	Protocol  Protocol
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
	ProtocolSource       string            `json:"protocol_source"`
	AvailableZen         bool              `json:"available_zen"`
	AvailableGo          bool              `json:"available_go"`
	Tier                 Tier              `json:"tier,omitempty"`
	Anonymous            bool              `json:"anonymous"`
	KeyTiers             []Tier            `json:"key_tiers,omitempty"`
	AnonymousEligibility AnonymousDecision `json:"anonymous_eligibility"`
	RouteError           string            `json:"route_error,omitempty"`
}

type modelCatalog struct {
	mu        sync.RWMutex
	zen       map[string]bool
	goModels  map[string]bool
	protocols map[string]Protocol
	updatedAt time.Time
	prefer    Tier
	metadata  *modelMetadataStore
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
	return &modelCatalog{zen: map[string]bool{}, goModels: map[string]bool{}, protocols: protocols, prefer: prefer}
}

func (c *modelCatalog) Replace(zen, goModels []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if zen != nil {
		c.zen = toSet(zen)
	}
	if goModels != nil {
		c.goModels = toSet(goModels)
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
	updatedAt := source.updatedAt
	source.mu.RUnlock()
	c.mu.Lock()
	c.zen, c.goModels, c.updatedAt = zen, goModels, updatedAt
	c.mu.Unlock()
}

func (c *modelCatalog) Route(model string, hasZenKeys, hasGoKeys, hasAnonymous bool) (modelRoute, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	protocol := c.protocols[model]
	if protocol == "" {
		protocol = inferProtocol(model)
	}
	keyTiers := c.keyTierOrderLocked(model, hasZenKeys, hasGoKeys)
	// OpenCode's public credential is a Zen-only lane. Every free model starts
	// there, even if the current catalog only advertises it on Go: an upstream
	// rejection will move the request into the authenticated fallback plan.
	decision := c.anonymousDecision(model)
	if hasAnonymous && decision.Allowed {
		return modelRoute{ID: model, Tier: TierZen, Protocol: protocol, Anonymous: true, KeyTiers: keyTiers}, nil
	}
	if len(keyTiers) > 0 {
		return modelRoute{ID: model, Tier: keyTiers[0], Protocol: protocol, KeyTiers: keyTiers}, nil
	}
	return modelRoute{}, fmt.Errorf("model %q is not available in the configured Zen or Go pools", model)
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
			return hasZenKeys && (catalogPending || c.zen[model])
		case TierGo:
			return hasGoKeys && (catalogPending || c.goModels[model])
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

func (c *modelCatalog) RouteForTier(model string, requested Protocol, tier Tier, hasZenKeys, hasGoKeys bool) (modelRoute, error) {
	if tier != TierZen && tier != TierGo {
		return modelRoute{}, fmt.Errorf("selected key tier must be zen or go")
	}
	c.mu.RLock()
	protocol := c.protocols[model]
	catalogPending := len(c.zen) == 0 && len(c.goModels) == 0
	available := catalogPending
	if tier == TierZen {
		available = available || c.zen[model]
		if !hasZenKeys {
			available = false
		}
	} else {
		available = available || c.goModels[model]
		if !hasGoKeys {
			available = false
		}
	}
	c.mu.RUnlock()
	if protocol == "" {
		protocol = inferProtocol(model)
	}
	if !available {
		return modelRoute{}, fmt.Errorf("model %q is not available in the selected %s key tier", model, tier)
	}
	return modelRoute{ID: model, Tier: tier, Protocol: protocol, KeyTiers: []Tier{tier}}, nil
}

func (c *modelCatalog) anonymousDecision(model string) AnonymousDecision {
	if c.metadata != nil {
		return c.metadata.Decide(model)
	}
	return AnonymousDecision{Allowed: isFreeModel(model), Source: "name_fallback_metadata_pending"}
}

func (c *modelCatalog) Diagnostic(model string, requested Protocol, hasZenKeys, hasGoKeys, hasAnonymous bool) ModelRouteDiagnostic {
	c.mu.RLock()
	protocol, explicit := c.protocols[model]
	zen, goModel := c.zen[model], c.goModels[model]
	c.mu.RUnlock()
	source := "configured"
	if !explicit {
		protocol = inferProtocol(model)
		source = "inferred"
	}
	diagnostic := ModelRouteDiagnostic{
		Model: model, RequestedProtocol: requested, NativeProtocol: protocol, ProtocolSource: source,
		AvailableZen: zen, AvailableGo: goModel, AnonymousEligibility: c.anonymousDecision(model),
	}
	route, err := c.Route(model, hasZenKeys, hasGoKeys, hasAnonymous)
	if err != nil {
		diagnostic.RouteError = err.Error()
		return diagnostic
	}
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
		models = append(models, model)
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
		if supportedModel(model) {
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

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func inferProtocol(model string) Protocol {
	m := strings.ToLower(model)
	// This compatibility model speaks Chat Completions despite being exposed
	// beside models whose names can imply newer OpenAI-style protocols. An
	// explicit models.protocols entry is resolved before this default.
	if m == "deepseek-v4-flash-free" {
		return ProtocolChat
	}
	for _, prefix := range []string{"claude-", "qwen"} {
		if strings.HasPrefix(m, prefix) {
			return ProtocolAnthropic
		}
	}
	for _, prefix := range []string{"gpt-", "o1", "o3", "o4", "grok-", "muse-"} {
		if strings.HasPrefix(m, prefix) {
			return ProtocolResponses
		}
	}
	return ProtocolChat
}

func supportedModel(model string) bool {
	m := strings.ToLower(model)
	return !strings.HasPrefix(m, "gemini-")
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
