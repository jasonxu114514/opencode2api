package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
	"opencode2api/internal/warp"
)

func TestIsTitleGenerationRequest_Comprehensive(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{
			name: "Legacy v1 OpenCode title prompt in user turn",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "user", "content": "Generate a title for this conversation:\nUser: Fix login bug"}
				]
			}`,
			expected: true,
		},
		{
			name: "New v2 OpenCode title prompt in system turn",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else: one line, no quotes, no trailing period."},
					{"role": "user", "content": "Implement auth middleware"}
				]
			}`,
			expected: true,
		},
		{
			name: "New v2 OpenCode title prompt variant with 'for it'",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You name conversations so the user can find them later in a list of past sessions. Reply with a title for it and nothing else: one line, no quotes, no trailing period."},
					{"role": "user", "content": "Fix typo"}
				]
			}`,
			expected: true,
		},
		{
			name: "New v2 auto-rename prompt",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You decide whether the session title still matches the conversation. Reply with KEEP or new title."},
					{"role": "user", "content": "Database migration script"}
				]
			}`,
			expected: true,
		},
		{
			name: "Normal user agent request with declared tools",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You are a coding assistant."},
					{"role": "user", "content": "List files in current directory"}
				],
				"tools": [
					{"type": "function", "function": {"name": "shell", "description": "run command"}}
				]
			}`,
			expected: false,
		},
		{
			name: "Tool-free regular Q&A request (must NOT be treated as title)",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "user", "content": "What is the capital of France?"}
				]
			}`,
			expected: false,
		},
		{
			name: "User message quotes title prompt in normal conversation (false positive prevention)",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You are a helpful coding assistant."},
					{"role": "user", "content": "Can you review this prompt for me: 'You name conversations so the user can find them later in a list of past sessions.'"}
				]
			}`,
			expected: false,
		},
		{
			name: "User message mentions legacy prompt in the middle of text",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "user", "content": "How does OpenCode generate a title for this conversation?"}
				]
			}`,
			expected: false,
		},
		{
			name: "Multi-turn conversation with assistant turn (must NOT be treated as title)",
			body: `{
				"model": "flint-40b-free",
				"messages": [
					{"role": "system", "content": "You name conversations so the user can find them later in a list of past sessions."},
					{"role": "user", "content": "Hi"},
					{"role": "assistant", "content": "Hello!"},
					{"role": "user", "content": "Now generate code"}
				]
			}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTitleGenerationBody([]byte(tt.body))
			if got != tt.expected {
				t.Errorf("isTitleGenerationBody() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestPrepareAnonymousBody_TitleVsAgentToolsInjection(t *testing.T) {
	v2TitleBody := []byte(`{
		"model": "flint-40b-free",
		"messages": [
			{"role": "system", "content": "You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else: one line, no quotes, no trailing period."},
			{"role": "user", "content": "Add Redis caching"}
		]
	}`)

	v1TitleBody := []byte(`{
		"model": "flint-40b-free",
		"messages": [
			{"role": "user", "content": "Generate a title for this conversation:\nUser: Add Redis caching"}
		]
	}`)

	regularAgentBody := []byte(`{
		"model": "flint-40b-free",
		"messages": [
			{"role": "user", "content": "Explain how Go channels work."}
		]
	}`)

	// 1. v2 Title body: must NOT have tools injected
	shapedV2, _ := prepareAnonymousBody(v2TitleBody, wire.Chat, false)
	var parsedV2 map[string]any
	if err := json.Unmarshal(shapedV2, &parsedV2); err != nil {
		t.Fatalf("Unmarshal shapedV2 error: %v", err)
	}
	if tools, ok := parsedV2["tools"].([]any); ok && len(tools) > 0 {
		t.Errorf("expected title request to stay tool-free, but got %d tools injected", len(tools))
	}

	// 2. v1 Title body: must NOT have tools injected
	shapedV1, _ := prepareAnonymousBody(v1TitleBody, wire.Chat, false)
	var parsedV1 map[string]any
	if err := json.Unmarshal(shapedV1, &parsedV1); err != nil {
		t.Fatalf("Unmarshal shapedV1 error: %v", err)
	}
	if tools, ok := parsedV1["tools"].([]any); ok && len(tools) > 0 {
		t.Errorf("expected v1 title request to stay tool-free, but got %d tools injected", len(tools))
	}

	// 3. Regular agent request without tools: MUST have tools injected
	shapedAgent, _ := prepareAnonymousBody(regularAgentBody, wire.Chat, false)
	var parsedAgent map[string]any
	if err := json.Unmarshal(shapedAgent, &parsedAgent); err != nil {
		t.Fatalf("Unmarshal shapedAgent error: %v", err)
	}
	tools, ok := parsedAgent["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Errorf("expected regular agent request to have official tools injected, got %v", parsedAgent["tools"])
	}
}

func TestSynthesizeTitle_UnicodeAndFallbacks(t *testing.T) {
	tests := []struct {
		name     string
		payload  map[string]any
		expected string
	}{
		{
			name: "Standard english prompt",
			payload: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "Fix authentication bug in middleware"},
				},
			},
			expected: "Fix authentication bug in middleware",
		},
		{
			name: "Unicode characters and truncation",
			payload: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "Türkçe başlık oluşturma ve kota sınırlaması çözümü"},
				},
			},
			expected: "Türkçe başlık oluşturma ve kota sınırlam",
		},
		{
			name: "Legacy prefix stripped cleanly",
			payload: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "Generate a title for this conversation:\nUser: Upgrade database schema to v3"},
				},
			},
			expected: "User: Upgrade database schema to v3",
		},
		{
			name: "Empty content safe fallback",
			payload: map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": "   "},
				},
			},
			expected: "New Session",
		},
		{
			name: "No user message safe fallback",
			payload: map[string]any{
				"messages": []any{
					map[string]any{"role": "system", "content": "System prompt only"},
				},
			},
			expected: "New Session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := synthesizeTitle(tt.payload)
			if got != tt.expected {
				t.Errorf("synthesizeTitle() = %q, expected %q", got, tt.expected)
			}
		})
	}
}

func TestWarpAnonymous_TitleRequestSingleAttemptFallbackOn429(t *testing.T) {
	// Mock 2 WARP exits that both return 429
	exit1, l1 := createMockWarpExit(t, "exit-1", "2a09:bac1:7280:18::1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota_exceeded"}`))
	})
	defer l1.Close()

	exit2, l2 := createMockWarpExit(t, "exit-2", "2a09:bac5:58c0:d2d::1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota_exceeded"}`))
	})
	defer l2.Close()

	pool := warp.NewTestPoolWithExits([]*warp.TestExit{exit1, exit2})

	cfg := config.Config{
		Anonymous: true,
		Retry: config.RetryConfig{
			MaxAttempts:    12, // Normally 12, but must be capped to 1 for title requests
			TimeoutSeconds: 5,
		},
		Warp: config.WarpConfig{
			Enabled:  true,
			PoolSize: 2,
			Stack:    "ipv6",
		},
		Upstream: config.UpstreamConfig{
			Anonymous: "http://[fd00::1]:80",
		},
		Performance: config.PerformanceConfig{
			FailureCooldownSeconds: 15,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor, pool)
	if err != nil {
		t.Fatalf("New gateway error: %v", err)
	}

	ctx := context.Background()
	route := models.Route{ID: "test-model", Protocol: wire.Chat}
	titleBody := []byte(`{
		"model": "test-model",
		"messages": [
			{"role": "system", "content": "You name conversations so the user can find them later in a list of past sessions. Reply with a title and nothing else: one line, no quotes, no trailing period."},
			{"role": "user", "content": "Refactor auth middleware"}
		],
		"stream": false
	}`)
	bodies := map[config.Tier][]byte{
		config.TierZen: titleBody,
	}
	ids := identity.RequestIDs{Request: "req-title-1", Session: "ses-title-1"}

	start := time.Now()
	resp, err, attempts := gw.doWarpAnonymousUpstream(ctx, route, bodies, ids, 0)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("expected fallback response without error, got %v", err)
	}
	if resp == nil {
		t.Fatalf("expected response, got nil")
	}
	defer resp.Body.Close()

	// Must stop after exactly 1 attempt so it does NOT burn the entire pool
	if attempts != 1 {
		t.Errorf("expected title request to try exactly 1 exit, got %d attempts", attempts)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected fallback status 200 OK, got %d", resp.StatusCode)
	}
	if duration > 1*time.Second {
		t.Errorf("expected quick response, took %v", duration)
	}

	// Read and verify synthesized JSON response
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	choices, ok := res["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices array in fallback response, got %v", res)
	}
	firstChoice := choices[0].(map[string]any)
	message := firstChoice["message"].(map[string]any)
	content := message["content"].(string)
	expectedContent := "Refactor auth middleware"
	if content != expectedContent {
		t.Errorf("expected synthesized title %q, got %q", expectedContent, content)
	}
}
