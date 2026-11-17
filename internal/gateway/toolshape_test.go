package gateway

import (
	"encoding/json"
	"testing"

	wire "opencode2api/internal/protocol"
)

func TestRenameAndCompleteToolsBindsCodexAdapters(t *testing.T) {
	var payload map[string]any
	raw := `{
		"tools": [
			{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
			{"type":"custom","name":"apply_patch"}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	renames, ok := renameAndCompleteTools(payload, wire.Responses, false)
	if !ok {
		t.Fatal("renameAndCompleteTools returned false")
	}
	for _, testCase := range []struct{ official, client string }{
		{"patch", "apply_patch"},
		{"shell", "exec_command"},
	} {
		declared, ok := renames.Official(testCase.official)
		if !ok || !sameToolName(declared, testCase.client) {
			t.Fatalf("official %q bound to %q, want %q", testCase.official, declared, testCase.client)
		}
		if _, ok := renames.ArgumentTransform(testCase.official); !ok {
			t.Fatalf("official %q has no argument transform", testCase.official)
		}
	}
	if !renames.Custom("apply_patch") {
		t.Fatal("apply_patch was not marked custom")
	}
	assertTransformJSON(t, renames, "patch", `{"patchText":"*** Begin Patch\n*** End Patch"}`, map[string]any{
		"input": "*** Begin Patch\n*** End Patch",
	})
	assertTransformJSON(t, renames, "shell", `{"command":"echo hi","workdir":"C:\\tmp"}`, map[string]any{
		"cmd":     "echo hi",
		"workdir": `C:\tmp`,
	})
}

func TestRenameAndCompleteToolsBindsTodoWrite(t *testing.T) {
	var payload map[string]any
	raw := `{
		"tools": [
			{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
			{"type":"function","name":"update_plan","parameters":{"type":"object","properties":{"plan":{"type":"array"}}}}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	renames, ok := renameAndCompleteTools(payload, wire.Responses, true)
	if !ok {
		t.Fatal("renameAndCompleteTools returned false")
	}
	declared, ok := renames.Official("todowrite")
	if !ok || !sameToolName(declared, "update_plan") {
		t.Fatalf("official todowrite bound to %q, want update_plan", declared)
	}
	if _, ok := renames.ArgumentTransform("todowrite"); !ok {
		t.Fatal("official todowrite has no argument transform")
	}
	assertTransformJSON(t, renames, "todowrite", `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"cancelled"}]}`, map[string]any{
		"plan": []any{map[string]any{"step": "a", "status": "in_progress"}},
	})
}

func assertTransformJSON(t *testing.T, renames *wire.ToolRenames, official, raw string, want map[string]any) {
	t.Helper()
	transform, ok := renames.ArgumentTransform(official)
	if !ok {
		t.Fatalf("%s: no argument transform", official)
	}
	got, err := transform(raw)
	if err != nil {
		t.Fatalf("%s: %v", official, err)
	}
	var gotValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("%s: invalid JSON %q: %v", official, got, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var wantValue any
	if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(gotValue)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s: got %s, want %s", official, gotJSON, wantJSON)
	}
}
