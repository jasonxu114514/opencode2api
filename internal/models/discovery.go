package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	wire "opencode2api/internal/protocol"
)

// CapabilitiesURL is OpenCode's machine-readable model catalog. It feeds
// FetchCapabilities and is also the OpenCode metadata endpoint the
// PricingRegistry reads. ZenDocsURL and GoDocsURL are the Zen and Go
// documentation pages whose endpoint tables FetchCapabilities reads with
// FetchProtocolDocs.
const (
	CapabilitiesURL = "https://models.opencode.ai/api.json"
	ZenDocsURL      = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
	GoDocsURL       = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/go.mdx"
	// V1Base is the v1 lane's origin: the /zen console (serving /zen/v1/*) that
	// expects the OpenCode 1.x request shape. It is the default keyed Zen
	// upstream, and the anonymous lane tries it before the v2 /inference
	// origin. It also serves the models listing.
	V1Base = "https://opencode.ai/zen"
)

// IsV1Upstream reports whether baseURL targets the v1 /zen gate, which expects
// the OpenCode 1.x request shape, rather than a v2 gate (/inference or
// /zen/go), which expects the OpenCode 2.x CLI shape.
func IsV1Upstream(baseURL string) bool {
	trimmed := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if strings.EqualFold(trimmed, strings.TrimRight(V1Base, "/")) {
		return true
	}
	return strings.HasSuffix(trimmed, "/zen")
}

// protocolDocEndpointPattern matches a row of a documentation endpoint table,
// "| name | `model-id` | `.../v1/<endpoint>` |", capturing the model ID and the
// endpoint.
var protocolDocEndpointPattern = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages|systemone)`")

// Capabilities is the per-tier result of FetchCapabilities: the native
// protocol of every model, the models whose SDK maps to no supported protocol,
// and the metadata of every listed model.
type Capabilities struct {
	Protocols   map[config.Tier]map[string]wire.Protocol
	Unsupported map[config.Tier]map[string]bool
	Metadata    map[config.Tier]map[string]Metadata
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

// Metadata carries the per-model capability fields surfaced through
// /v1/models so consumers (harnesses like Pi or jcode) get real context
// windows and feature flags from the catalog instead of guessing. It is
// purely additive: routing does not depend on any of these fields.
type Metadata struct {
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxInput         int      `json:"max_input,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Reasoning        bool     `json:"reasoning,omitempty"`
	ToolCall         bool     `json:"tool_call,omitempty"`
	StructuredOutput bool     `json:"structured_output,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	// Cost is filled from the OpenCode pricing metadata at read time, in USD per
	// million tokens.
	Cost *Cost `json:"cost,omitempty"`
}

// Cost is the published per-million-token price of a model. A nil field means
// the metadata did not state it.
type Cost struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func (m *capabilityModel) metadata() Metadata {
	md := Metadata{
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

// FetchCapabilities reads OpenCode's machine-readable provider catalog from
// endpoint (normally CapabilitiesURL). Unlike /v1/models, this source includes
// the SDK selected for each model, which is the upstream's protocol
// declaration. The Zen and Go endpoint tables then refine it. Protocol
// discovery is data-driven: no model IDs are embedded here (the fixed SOTA
// rotation list in config.SOTAModels is unrelated). It fails when the catalog
// cannot be read or yields no Zen or Go model.
func FetchCapabilities(ctx context.Context, client *http.Client, endpoint string) (Capabilities, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Capabilities{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpx.V2UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return Capabilities{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Capabilities{}, fmt.Errorf("OpenCode capability endpoint returned HTTP %d", resp.StatusCode)
	}
	var providers map[string]capabilityProvider
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(&providers); err != nil {
		return Capabilities{}, err
	}
	result := Capabilities{
		Protocols:   map[config.Tier]map[string]wire.Protocol{config.TierZen: {}, config.TierGo: {}},
		Unsupported: map[config.Tier]map[string]bool{config.TierZen: {}, config.TierGo: {}},
		Metadata:    map[config.Tier]map[string]Metadata{config.TierZen: {}, config.TierGo: {}},
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
	// route), so a listed endpoint wins over the SDK and clears an unsupported
	// mark. A table that cannot be fetched is skipped. This remains
	// data-driven: no model IDs are embedded here.
	for _, doc := range []struct {
		tier config.Tier
		url  string
	}{
		{config.TierZen, ZenDocsURL},
		{config.TierGo, GoDocsURL},
	} {
		protocols, err := FetchProtocolDocs(ctx, client, doc.url)
		if err != nil {
			continue
		}
		for modelID, protocol := range protocols {
			result.Protocols[doc.tier][modelID] = protocol
			delete(result.Unsupported[doc.tier], modelID)
		}
	}
	if len(result.Protocols[config.TierZen]) == 0 && len(result.Protocols[config.TierGo]) == 0 && len(result.Unsupported[config.TierZen]) == 0 && len(result.Unsupported[config.TierGo]) == 0 {
		return Capabilities{}, errors.New("OpenCode capability endpoint returned no Zen or Go models")
	}
	return result, nil
}

// FetchProtocolDocs parses the endpoint table of an OpenCode documentation
// page into a model-to-protocol map, deriving the protocol from the endpoint
// path of each row. It fails when no row matches. A nil client uses
// http.DefaultClient.
func FetchProtocolDocs(ctx context.Context, client *http.Client, endpoint string) (map[string]wire.Protocol, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain, text/markdown, */*")
	req.Header.Set("User-Agent", httpx.V2UserAgent())
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
	result := make(map[string]wire.Protocol)
	for _, line := range strings.Split(string(body), "\n") {
		match := protocolDocEndpointPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		modelID := strings.TrimSpace(match[1])
		if modelID == "" || strings.ContainsAny(modelID, " `|") {
			continue
		}
		var protocol wire.Protocol
		switch match[2] {
		case "chat/completions":
			protocol = wire.Chat
		case "responses":
			protocol = wire.Responses
		case "messages":
			protocol = wire.Anthropic
		case "systemone":
			protocol = wire.SystemOne
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

// capabilityTier maps a catalog provider to a tier by its ID and API URL: an
// opencode-go provider or a /go/ URL is Go, any other OpenCode provider or a
// /zen/ URL is Zen, and every other provider is skipped.
func capabilityTier(providerID, api string) (config.Tier, bool) {
	value := strings.ToLower(strings.TrimSpace(providerID + " " + api))
	if strings.Contains(value, "opencode-go") || strings.Contains(value, "/go/") {
		return config.TierGo, true
	}
	if strings.Contains(value, "opencode") || strings.Contains(value, "/zen/") {
		return config.TierZen, true
	}
	return "", false
}

// protocolForSDK maps the AI SDK package a model is served through to its wire
// protocol. It reports false for a package the gateway cannot speak.
func protocolForSDK(npm string) (wire.Protocol, bool) {
	value := strings.ToLower(strings.TrimSpace(npm))
	switch {
	case strings.Contains(value, "anthropic"):
		return wire.Anthropic, true
	case value == "@ai-sdk/openai" || strings.HasSuffix(value, "/openai"):
		return wire.Responses, true
	case strings.Contains(value, "openai-compatible"):
		return wire.Chat, true
	default:
		return "", false
	}
}

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// FetchModels lists the model IDs at baseURL's /v1/models with key, sending
// the v1 or v2 User-Agent that matches the lane. It also returns the HTTP
// status, 0 when no response arrived, so callers can judge the proxy. An empty
// list is an error.
func FetchModels(ctx context.Context, client *http.Client, baseURL, key string) ([]string, int, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, trimmed+"/v1/models", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if IsV1Upstream(trimmed) {
		req.Header.Set("User-Agent", httpx.V1UserAgent())
	} else {
		req.Header.Set("User-Agent", httpx.V2UserAgent())
	}
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
