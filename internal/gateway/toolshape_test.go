package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	wire "opencode2api/internal/protocol"
)

// TestPrepareAnonymousBodyHidesClientToolNames pins the fix for the free tier's
// client detection: a Claude Code request that declares both of these tools is
// answered with FreeUsageLimitError, and renaming either name lifts the
// refusal. The shaped body must therefore never spell a client tool name.
func TestPrepareAnonymousBodyHidesClientToolNames(t *testing.T) {
	body := []byte(`{
		"model": "deepseek-v4.1-flash-free",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": false,
		"tools": [
			{"type": "function", "function": {"name": "Read", "description": "Read a file.",
			 "parameters": {"type": "object", "properties": {"file_path": {"type": "string"}}}}},
			{"type": "function", "function": {"name": "ReportFindings", "description": "Report findings.",
			 "parameters": {"type": "object", "properties": {}}}},
			{"type": "function", "function": {"name": "ScheduleWakeup", "description": "Schedule a wakeup.",
			 "parameters": {"type": "object", "properties": {}}}}
		]
	}`)

	shaped, renames := prepareAnonymousBody(body, wire.Chat, false)
	if len(shaped) == 0 {
		t.Fatal("no shaped body")
	}
	text := string(shaped)
	// The two Claude Code markers are distinctive enough to assert over the
	// whole body; a generic name like Read also appears in the official prompt
	// prose, so the tool list is what must stay free of it.
	for _, marker := range []string{"ReportFindings", "ScheduleWakeup"} {
		if strings.Contains(text, marker) {
			t.Errorf("shaped body still spells the client tool %q", marker)
		}
	}
	for _, client := range []string{"Read", "ReportFindings", "ScheduleWakeup"} {
		if _, ok := renames.Wire(client); !ok {
			t.Errorf("no wire name recorded for %q", client)
		}
	}

	var payload struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatalf("shaped body is not JSON: %v", err)
	}
	if !payload.Stream {
		t.Error("shaped body is not streaming")
	}
	names := make(map[string]bool, len(payload.Tools))
	for _, tool := range payload.Tools {
		names[tool.Function.Name] = true
	}
	// The official set is what makes the request read as an OpenCode session;
	// dropping it is refused just like keeping a foreign tool name.
	for _, official := range []string{"read", "edit", "shell"} {
		if !names[official] {
			t.Errorf("shaped body is missing the official tool %q", official)
		}
	}
	for client := range map[string]bool{"Read": true, "ReportFindings": true, "ScheduleWakeup": true} {
		wireName, _ := renames.Wire(client)
		if !names[wireName] {
			t.Errorf("client tool %q is not declared under its wire name %q", client, wireName)
		}
	}
}

// TestPrepareAnonymousBodyRestoresToolCalls keeps the response side honest: a
// call the model makes to a renamed client tool comes back under the name the
// client declared.
func TestPrepareAnonymousBodyRestoresToolCalls(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"ScheduleWakeup","parameters":{"type":"object","properties":{}}}}]}`)
	_, renames := prepareAnonymousBody(body, wire.Chat, false)
	wireName, ok := renames.Wire("ScheduleWakeup")
	if !ok {
		t.Fatal("no wire name recorded")
	}
	if got := renames.Restore(wireName); got != "ScheduleWakeup" {
		t.Errorf("Restore(%q) = %q, want ScheduleWakeup", wireName, got)
	}
}
