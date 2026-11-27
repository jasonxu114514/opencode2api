package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// customRenames builds the rename set a shaped request would carry for a client
// that declared one Responses custom tool.
func customRenames() *ToolRenames {
	renames := NewToolRenames()
	renames.Add("apply_patch", ClientToolWireName("apply_patch", nil))
	renames.MarkCustom("apply_patch")
	renames.BindOfficial("patch", "apply_patch")
	return renames
}

// TestConvertResponseCustomTool checks a Chat tool call for a client custom tool
// is re-encoded as a custom_tool_call carrying the freeform input.
func TestConvertResponseCustomTool(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","model":"m","created":1,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"patch","arguments":"{\"patchText\":\"*** Begin Patch\\n*** End Patch\\n\"}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`)
	out, err := ConvertResponse(Chat, Responses, body, customRenames())
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	output, _ := payload["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output items = %d, want 1: %s", len(output), out)
	}
	item, _ := output[0].(map[string]any)
	if got := item["type"]; got != "custom_tool_call" {
		t.Fatalf("item type = %v, want custom_tool_call: %s", got, out)
	}
	if got := item["name"]; got != "apply_patch" {
		t.Fatalf("item name = %v, want apply_patch", got)
	}
	if got := item["input"]; got != "*** Begin Patch\n*** End Patch\n" {
		t.Fatalf("item input = %q, want the freeform patch text", got)
	}
	if _, exists := item["arguments"]; exists {
		t.Fatalf("custom tool call still carries arguments: %s", out)
	}
}

// TestRenameStreamReaderCustomTool checks a Responses stream is rewritten so a
// client custom tool's call arrives as custom_tool_call with input events.
func TestRenameStreamReaderCustomTool(t *testing.T) {
	in := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"patch"}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"patchText\":\"*** Begin"}`,
		``,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"patchText\":\"*** Begin Patch\\n*** End Patch\\n\"}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"patchText\":\"*** Begin Patch\\n*** End Patch\\n\"}","call_id":"call_1","name":"patch"}}`,
		``,
	}, "\n")
	out, err := io.ReadAll(RenameStreamReader(strings.NewReader(in), Responses, customRenames()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(out)
	if strings.Contains(text, `"function_call"`) || strings.Contains(text, "function_call_arguments") {
		t.Fatalf("stream still carries function_call events:\n%s", text)
	}
	for _, want := range []string{`"custom_tool_call"`, `"response.custom_tool_call_input.done"`, `"input":"*** Begin Patch\n*** End Patch\n"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream is missing %s:\n%s", want, text)
		}
	}
}

// TestPrepareRequestCustomTool checks a Responses custom tool is bridged to a
// single-string function on the Chat lane instead of being dropped.
func TestPrepareRequestCustomTool(t *testing.T) {
	var input map[string]any
	raw := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}]}`
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := PrepareRequest(Responses, Chat, input, "https://opencode.ai/zen")
	if err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1: %v", len(tools), out["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	if got := function["name"]; got != "apply_patch" {
		t.Fatalf("tool name = %v, want apply_patch", got)
	}
	schema, _ := function["parameters"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool schema has no input property: %v", schema)
	}
}
