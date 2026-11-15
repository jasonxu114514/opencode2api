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
	if got := item["arguments"]; got != `{"input":"P"}` {
		t.Fatalf("arguments = %v, want the transformed input envelope", got)
	}
}

func TestCustomToolInput(t *testing.T) {
	if got := customToolInput(`{"input":"hello"}`); got != "hello" {
		t.Fatalf("customToolInput = %q, want hello", got)
	}
	if got := customToolInput(`raw`); got != "raw" {
		t.Fatalf("customToolInput = %q, want the original text", got)
	}
}
