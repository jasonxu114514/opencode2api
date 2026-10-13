package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode2api/internal/config"
)

func coreToolset() []any {
	names := []string{"bash", "edit", "glob", "grep", "read"}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Agent tool " + name,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return tools
}

func baseZenBody(t *testing.T, messages []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":            "test-model",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
		"reasoning_effort": "high",
		"tools":            coreToolset(),
		"messages":         messages,
	})
	if err != nil {
		t.Fatalf("marshal base body: %v", err)
	}
	return body
}

func decodeContinuation(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal continuation body: %v", err)
	}
	return payload
}

func continuationMessages(t *testing.T, payload map[string]any) []any {
	t.Helper()
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("continuation body has no messages: %v", payload)
	}
	return messages
}

func messageField(t *testing.T, message any, key string) any {
	t.Helper()
	entry, ok := message.(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %T", message)
	}
	return entry[key]
}

func TestBuildContinuationBody_trailingUser(t *testing.T) {
	base := baseZenBody(t, []any{map[string]any{"role": "user", "content": "hi"}})
	out, err := buildContinuationBody(base, "partial text")
	if err != nil {
		t.Fatalf("buildContinuationBody: %v", err)
	}
	payload := decodeContinuation(t, out)
	messages := continuationMessages(t, payload)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if got := messageField(t, messages[1], "role"); got != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", got)
	}
	if got := messageField(t, messages[1], "content"); got != "partial text" {
		t.Fatalf("messages[1].content = %v, want partial text", got)
	}
	if got := messageField(t, messages[2], "role"); got != "user" {
		t.Fatalf("messages[2].role = %v, want user", got)
	}
	if got := messageField(t, messages[2], "content"); got != continuationInstruction {
		t.Fatalf("messages[2].content = %v, want the continuation instruction", got)
	}
	if got := payload["stream"]; got != true {
		t.Fatalf("stream = %v, want true", got)
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not preserved: %v", payload["stream_options"])
	}
	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 5 {
		t.Fatalf("tools not preserved: %v", payload["tools"])
	}
}

func TestBuildContinuationBody_trailingAssistantMerged(t *testing.T) {
	base := baseZenBody(t, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "X"},
	})
	out, err := buildContinuationBody(base, "partial")
	if err != nil {
		t.Fatalf("buildContinuationBody: %v", err)
	}
	payload := decodeContinuation(t, out)
	messages := continuationMessages(t, payload)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if got := messageField(t, messages[1], "role"); got != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", got)
	}
	if got := messageField(t, messages[1], "content"); got != "Xpartial" {
		t.Fatalf("messages[1].content = %v, want Xpartial", got)
	}
	if got := messageField(t, messages[2], "content"); got != continuationInstruction {
		t.Fatalf("messages[2].content = %v, want the continuation instruction", got)
	}
	assistants := 0
	for _, message := range messages {
		if messageField(t, message, "role") == "assistant" {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant message count = %d, want exactly 1 before the instruction", assistants)
	}
}

func TestResumeSegment_rejectsBadPartial(t *testing.T) {
	resumer := &gatewayStreamResumer{}
	if _, err := resumer.ResumeSegment(""); err == nil {
		t.Fatal("empty partial must be rejected")
	}
	if _, err := resumer.ResumeSegment(strings.Repeat("a", maxContinuationPartialBytes+1)); err == nil {
		t.Fatal("oversized partial must be rejected")
	}
}

func TestStreamResumeLimit_defaultsToOne(t *testing.T) {
	g := &Gateway{cfg: config.Config{Performance: config.PerformanceConfig{StreamContinuationMax: 0}}}
	if got := g.streamResumeLimit(); got != 1 {
		t.Fatalf("streamResumeLimit() = %d for 0, want 1", got)
	}
	g.cfg.Performance.StreamContinuationMax = 3
	if got := g.streamResumeLimit(); got != 3 {
		t.Fatalf("streamResumeLimit() = %d for 3, want 3", got)
	}
}
