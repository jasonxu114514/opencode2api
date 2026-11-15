package protocol

import "testing"

func TestNormalizeChatEmptyContent(t *testing.T) {
	input := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": nil},
			map[string]any{"role": "user"},
			map[string]any{"role": "assistant", "content": "keep"},
		},
	}
	if !normalizeChatEmptyContent(input) {
		t.Fatal("expected a change")
	}
	messages := input["messages"].([]any)
	for index, want := range []string{"", "", "keep"} {
		message := messages[index].(map[string]any)
		if got := message["content"]; got != want {
			t.Fatalf("message %d content = %v, want %q", index, got, want)
		}
	}
	if normalizeChatEmptyContent(map[string]any{"messages": []any{map[string]any{"content": "x"}}}) {
		t.Fatal("unchanged messages reported a change")
	}
	if normalizeChatEmptyContent(map[string]any{}) {
		t.Fatal("missing messages reported a change")
	}
}

func TestDecodeResponsesToolChoiceCustom(t *testing.T) {
	choice := decodeResponsesToolChoice(map[string]any{"type": "custom", "name": "apply_patch"})
	if choice.Mode != "named" || choice.Name != "apply_patch" {
		t.Fatalf("choice = %+v, want named apply_patch", choice)
	}
}
