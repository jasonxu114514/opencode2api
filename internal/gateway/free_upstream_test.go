package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

func TestFreeTierToolsDefinition(t *testing.T) {
	var tools map[string]opencodeToolDefinition
	if err := json.Unmarshal(opencodeToolsJSON, &tools); err != nil {
		t.Fatalf("failed to unmarshal opencodeToolsJSON: %v", err)
	}

	shell, ok := tools["shell"]
	if !ok {
		t.Fatal("shell tool definition not found in opencodeToolsJSON")
	}

	// Verify no Linux/bash pollution
	if strings.Contains(shell.Description, "Commands run on Linux using bash.") {
		t.Errorf("shell tool description still contains platform pollution: %q", shell.Description)
	}

	expectedPrefix := "Execute a shell command and return its output. Quote file paths containing spaces or special characters."
	if !strings.HasPrefix(shell.Description, expectedPrefix) {
		t.Errorf("unexpected shell description prefix, got: %q", shell.Description)
	}

	// Verify patch tool exists
	if _, ok := tools["patch"]; !ok {
		t.Error("patch tool should exist in opencodeToolsJSON")
	}
}

func TestSetV2Headers(t *testing.T) {
	header := make(http.Header)
	ids := identity.RequestIDs{
		Session:       "ses_0123456789abCdefGhijKlmnOp",
		ParentSession: "ses_0123456789abCdefGhijKlmnRoot",
		Project:       "global",
	}

	setV2Headers(header, wire.Anthropic, ids)

	expectedParent := identity.CanonicalSessionID(ids.ParentSession)
	// Check Parent session headers
	if parent := header.Get("x-opencode-parent-session-id"); parent != expectedParent {
		t.Errorf("expected x-opencode-parent-session-id %q, got %q", expectedParent, parent)
	}
	if parent := header.Get("x-parent-session-id"); parent != expectedParent {
		t.Errorf("expected x-parent-session-id %q, got %q", expectedParent, parent)
	}

	// Check affinity headers (should point to root parent session)
	if affinity := header.Get("x-session-affinity"); affinity != expectedParent {
		t.Errorf("expected x-session-affinity to be parent %q, got %q", expectedParent, affinity)
	}
	if sessionID := header.Get("x-opencode-session-id"); sessionID != ids.Session {
		t.Errorf("expected x-opencode-session-id to be current session %q, got %q", ids.Session, sessionID)
	}

	// Check anthropic-beta and anthropic-version
	beta := header.Get("anthropic-beta")
	if beta != "interleaved-thinking-2025-05-14" {
		t.Errorf("unexpected anthropic-beta: %q", beta)
	}
	if version := header.Get("anthropic-version"); version != "2023-06-01" {
		t.Errorf("unexpected anthropic-version: %q", version)
	}
}

func TestUpstreamRequestAnthropicBetaQuery(t *testing.T) {
	ctx := context.Background()
	req, err := upstreamRequest(ctx, "https://opencode.ai/inference", wire.Anthropic, []byte(`{}`), identity.RequestIDs{Session: "ses_123"}, "public")
	if err != nil {
		t.Fatalf("upstreamRequest failed: %v", err)
	}

	expectedSuffix := "/v1/messages?beta=true"
	if !strings.HasSuffix(req.URL.String(), expectedSuffix) {
		t.Errorf("expected URL ending in %q, got %q", expectedSuffix, req.URL.String())
	}
}

func TestAnonymousUpstreamsOrder(t *testing.T) {
	g := &Gateway{
		cfg: config.Config{
			Upstream: config.UpstreamConfig{
				Anonymous: "https://opencode.ai/inference",
			},
		},
	}

	upstreams := g.anonymousUpstreams()
	if len(upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(upstreams))
	}
	if upstreams[0] != "https://opencode.ai/inference" {
		t.Errorf("expected first upstream to be v2 /inference, got %q", upstreams[0])
	}
}
