package goauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"opencode2api/internal/protocol"
)

type remoteSettings struct {
	BaseURL string `json:"baseURL"`
}

type remoteModel struct {
	ModelID  string         `json:"modelID"`
	Package  string         `json:"package"`
	Settings remoteSettings `json:"settings"`
	Disabled bool           `json:"disabled"`
}

type remoteProvider struct {
	Package  string                 `json:"package"`
	Settings remoteSettings         `json:"settings"`
	Disabled bool                   `json:"disabled"`
	Models   map[string]remoteModel `json:"models"`
}

func (client *Client) Discover(ctx context.Context, credential Credential) (map[string]Route, error) {
	if err := credential.Validate(); err != nil {
		return nil, err
	}
	body, err := client.request(ctx, http.MethodGet, "/api/v2/config", nil, &credential)
	if err != nil {
		return nil, err
	}
	var response struct {
		Providers map[string]remoteProvider `json:"providers"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Providers == nil {
		return nil, &OAuthError{Code: "invalid_response"}
	}
	routes := make(map[string]Route)
	ambiguous := make(map[string]bool)
	for _, provider := range response.Providers {
		if provider.Disabled {
			continue
		}
		for name, model := range provider.Models {
			if model.Disabled || !validOpaque(name, 1024) || ambiguous[name] {
				continue
			}
			modelID := model.ModelID
			if modelID == "" {
				modelID = name
			}
			if !validOpaque(modelID, 1024) {
				continue
			}
			packageName := model.Package
			if packageName == "" {
				packageName = provider.Package
			}
			wireProtocol, ok := packageProtocol(packageName)
			if !ok {
				continue
			}
			baseURL := model.Settings.BaseURL
			if baseURL == "" {
				baseURL = provider.Settings.BaseURL
			}
			baseURL, ok = normalizeBase(baseURL, wireProtocol)
			if !ok {
				continue
			}
			route := Route{ModelID: modelID, Protocol: wireProtocol, BaseURL: baseURL}
			if existing, exists := routes[name]; exists && existing != route {
				delete(routes, name)
				ambiguous[name] = true
				continue
			}
			routes[name] = route
		}
	}
	if len(routes) == 0 {
		return nil, &OAuthError{Code: "no_go_subscription"}
	}
	return routes, nil
}

func packageProtocol(packageName string) (protocol.Protocol, bool) {
	if strings.HasPrefix(packageName, "aisdk:@ai-sdk/") {
		packageName = strings.TrimPrefix(packageName, "aisdk:")
	}
	if strings.HasPrefix(packageName, "@opencode-ai/ai/providers/") {
		packageName = "@opencode/ai/providers/" + strings.TrimPrefix(packageName, "@opencode-ai/ai/providers/")
	}
	switch packageName {
	case "@ai-sdk/openai-compatible", "@opencode/ai/providers/openai-compatible", "@opencode/ai/providers/openai/chat":
		return protocol.Chat, true
	case "@ai-sdk/openai", "@opencode/ai/providers/openai", "@opencode/ai/providers/openai/responses", "@opencode/ai/providers/openai-compatible/responses", "@opencode/ai/providers/openai-compatible-responses":
		return protocol.Responses, true
	case "@ai-sdk/anthropic", "@opencode/ai/providers/anthropic", "@opencode/ai/providers/anthropic-compatible":
		return protocol.Anthropic, true
	default:
		return "", false
	}
}

func normalizeBase(raw string, wireProtocol protocol.Protocol) (string, bool) {
	if wireProtocol != protocol.Chat && wireProtocol != protocol.Responses && wireProtocol != protocol.Anthropic {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !officialURL(parsed) || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", false
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "/inference/go" {
		if wireProtocol == protocol.Anthropic {
			path += "/anthropic"
		} else {
			path += "/openai"
		}
	}
	switch path {
	case "/inference/go/openai", "/inference/go/openai/v1":
		if wireProtocol == protocol.Anthropic {
			return "", false
		}
		return "https://opencode.ai/inference/go/openai", true
	case "/inference/go/anthropic", "/inference/go/anthropic/v1":
		if wireProtocol != protocol.Anthropic {
			return "", false
		}
		return "https://opencode.ai/inference/go/anthropic", true
	default:
		return "", false
	}
}
