package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureCatalog mirrors the real models.opencode.ai/api.json shape: providers
// keyed by ID, each model carrying limit/reasoning/flags plus a NESTED
// modalities object. tier-split-model intentionally carries different limits
// per tier, reproducing the glm-5/minimax-m3 class of bug where a tier-blind
// metadata map advertises the wrong tier's context window.
const fixtureCatalog = `{
  "opencode": {
    "id": "opencode",
    "api": "https://opencode.ai/zen/v1",
    "npm": "@ai-sdk/openai-compatible",
    "models": {
      "tier-split-model": {
        "id": "tier-split-model",
        "limit": {"context": 1000000, "input": 990000, "output": 128000},
        "reasoning": true,
        "tool_call": true,
        "structured_output": false,
        "modalities": {"input": ["text", "image"], "output": ["text"]}
      },
      "zen-only-model": {
        "limit": {"context": 200000, "output": 32000},
        "reasoning": false,
        "tool_call": true,
        "structured_output": true,
        "modalities": {"input": ["text"], "output": ["text"]}
      }
    }
  },
  "opencode-go": {
    "id": "opencode-go",
    "api": "https://opencode.ai/go/v1",
    "npm": "@ai-sdk/openai-compatible",
    "models": {
      "tier-split-model": {
        "id": "tier-split-model",
        "limit": {"context": 200000, "input": 190000, "output": 32000},
        "reasoning": true,
        "tool_call": true,
        "structured_output": false,
        "modalities": {"input": ["text"], "output": ["text"]}
      }
    }
  }
}`

// fixtureRoundTripper serves the catalog fixture for the capability endpoint
// and fails everything else immediately, keeping protocol-docs fallback
// fetches hermetic instead of hitting the network.
type fixtureRoundTripper struct {
	catalogURL string
}

func (f fixtureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != f.catalogURL {
		return nil, &testDNSError{url: req.URL.String()}
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(fixtureCatalog)),
		Header:     make(http.Header),
	}, nil
}

type testDNSError struct{ url string }

func (e *testDNSError) Error() string { return "blocked test request to " + e.url }

func testParseCapabilities(t *testing.T) protocolCapabilities {
	t.Helper()
	const catalogURL = "https://example.invalid/api.json"
	client := &http.Client{Transport: fixtureRoundTripper{catalogURL: catalogURL}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	caps, err := fetchProtocolCapabilities(ctx, client, catalogURL)
	if err != nil {
		t.Fatalf("fetchProtocolCapabilities: %v", err)
	}
	return caps
}

func TestNestedModalitiesParsed(t *testing.T) {
	caps := testParseCapabilities(t)
	md := caps.Metadata[TierZen]["tier-split-model"]
	if len(md.InputModalities) != 2 || md.InputModalities[0] != "text" || md.InputModalities[1] != "image" {
		t.Fatalf("zen input modalities = %v, want [text image]", md.InputModalities)
	}
	if len(md.OutputModalities) != 1 || md.OutputModalities[0] != "text" {
		t.Fatalf("zen output modalities = %v, want [text]", md.OutputModalities)
	}
	if !md.Reasoning || !md.ToolCall || md.StructuredOutput {
		t.Fatalf("zen flags = %+v, want reasoning+tool_call", md)
	}
}

func TestTierSplitMetadataKeptSeparate(t *testing.T) {
	caps := testParseCapabilities(t)
	zen := caps.Metadata[TierZen]["tier-split-model"]
	goMd := caps.Metadata[TierGo]["tier-split-model"]
	if zen.ContextWindow != 1000000 {
		t.Fatalf("zen context = %d, want 1000000", zen.ContextWindow)
	}
	if goMd.ContextWindow != 200000 {
		t.Fatalf("go context = %d, want 200000", goMd.ContextWindow)
	}
	if zen.MaxOutput != 128000 || goMd.MaxOutput != 32000 {
		t.Fatalf("max_output zen=%d go=%d, want 128000/32000", zen.MaxOutput, goMd.MaxOutput)
	}
}

func TestMetadataForTierSelection(t *testing.T) {
	caps := testParseCapabilities(t)
	cat := newModelCatalog(TierZen, nil)
	cat.ReplaceWithCapabilities(
		[]string{"tier-split-model"}, []string{"tier-split-model"},
		caps.Protocols, caps.Unsupported, caps.Metadata,
	)
	if got := cat.MetadataForTier("tier-split-model", TierZen).ContextWindow; got != 1000000 {
		t.Fatalf("zen selection = %d, want 1000000", got)
	}
	if got := cat.MetadataForTier("tier-split-model", TierGo).ContextWindow; got != 200000 {
		t.Fatalf("go selection = %d, want 200000", got)
	}
	if got := cat.MetadataForTier("missing-model", TierZen).ContextWindow; got != 0 {
		t.Fatalf("unknown model = %d, want zero value", got)
	}
}

func TestMetadataCacheRoundTripV3(t *testing.T) {
	caps := testParseCapabilities(t)
	cat := newModelCatalog(TierZen, nil)
	cat.SetCachePath(filepath.Join(t.TempDir(), "cache.models.catalog.json"))
	cat.ReplaceWithCapabilities(
		[]string{"tier-split-model", "zen-only-model"}, []string{"tier-split-model"},
		caps.Protocols, caps.Unsupported, caps.Metadata,
	)
	if err := cat.SaveCache(); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	restored := newModelCatalog(TierZen, nil)
	if err := restored.LoadCache(cat.cachePath); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got := restored.MetadataForTier("tier-split-model", TierGo).ContextWindow; got != 200000 {
		t.Fatalf("restored go context = %d, want 200000", got)
	}
	restoredMd := restored.MetadataForTier("tier-split-model", TierZen)
	if len(restoredMd.InputModalities) != 2 || restoredMd.MaxOutput != 128000 {
		t.Fatalf("restored zen metadata = %+v, want modalities+128k output", restoredMd)
	}
}

func TestLegacyV2CacheRejected(t *testing.T) {
	// A pre-tier-split cache (flat metadata map, schema 2) must be refused
	// rather than misread: flat entries cannot be attributed to a tier.
	path := filepath.Join(t.TempDir(), "v2.models.catalog.json")
	legacy := map[string]any{
		"schema_version": 2,
		"updated_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"zen":            []string{"tier-split-model"},
		"go":             []string{},
		"metadata": map[string]any{
			"tier-split-model": map[string]any{"context_window": 1000000},
		},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cat := newModelCatalog(TierZen, nil)
	// The flat v2 metadata shape cannot even decode into the per-tier v3
	// struct, so rejection surfaces as a decode error before the explicit
	// version gate. Either way the stale cache is refused and the gateway
	// falls back to a live refresh — it must never be half-read.
	if err := cat.LoadCache(path); err == nil {
		t.Fatal("LoadCache accepted schema v2, want rejection")
	}
}
