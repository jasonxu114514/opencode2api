package protocol

import "testing"

func TestRenameToolInMapAppliesArgumentTransform(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("apply_patch", "clint_x")
	renames.BindOfficial("patch", "apply_patch")
	renames.MarkCustom("apply_patch")
	renames.SetArgumentTransform("patch", func(string) (string, error) {
		return customToolCallArguments("P"), nil
	})
	payload := map[string]any{
		"output": []any{
			map[string]any{"type": "function_call", "name": "patch", "arguments": `{"patchText":"P"}`},
		},
	}
	if !renameToolInMap(payload, Responses, renames) {
		t.Fatal("renameToolInMap reported no change")
	}
	item := payload["output"].([]any)[0].(map[string]any)
	if got := item["name"]; got != "apply_patch" {
		t.Fatalf("name = %v, want apply_patch", got)
	}
	if got := item["type"]; got != "custom_tool_call" {
		t.Fatalf("type = %v, want custom_tool_call", got)
	}
	if got := item["input"]; got != "P" {
		t.Fatalf("input = %v, want the transformed freeform text", got)
	}
	if _, exists := item["arguments"]; exists {
		t.Fatal("custom tool call still carries arguments")
	}
}

func TestCustomToolInput(t *testing.T) {
	if got := freeformToolInput(`{"input":"hello"}`); got != "hello" {
		t.Fatalf("freeformToolInput = %q, want hello", got)
	}
	if got := freeformToolInput(`raw`); got != "raw" {
		t.Fatalf("freeformToolInput = %q, want the original text", got)
	}
}
