package protocol

import (
	"encoding/json"
	"testing"
)

// A Responses turn can carry its assistant text in a message item that follows
// the function_call items of the same turn. Bridging that to Chat must keep the
// text and the calls in one assistant message: an assistant message between the
// calls and their results is not representable and was rejected as
// "assistant message appears before tool results".
func TestDecodeResponsesAssistantTextAfterToolCalls(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "list the docker dir"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "shell", "arguments": `{"command":"ls"}`},
			map[string]any{"type": "function_call", "call_id": "call_b", "name": "shell", "arguments": `{"command":"pwd"}`},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "I will inspect the directory first."},
			}},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "a b c"},
			map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "/tmp"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "continue"},
			}},
		},
	}
	request, err := decodeBridgeRequest(Responses, input)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(request.Messages) != 4 {
		t.Fatalf("got %d bridge messages, want 4", len(request.Messages))
	}
	assistant := request.Messages[1]
	if assistant.Role != "assistant" {
		t.Fatalf("messages[1] role = %q, want assistant", assistant.Role)
	}
	var calls, texts int
	for _, block := range assistant.Blocks {
		switch block.Kind {
		case "tool_call":
			calls++
		case "text":
			texts++
		}
	}
	if calls != 2 || texts != 1 {
		t.Fatalf("assistant blocks = %d calls / %d texts, want 2 / 1", calls, texts)
	}
	if _, err := encodeBridgeRequest(Chat, request); err != nil {
		t.Fatalf("encode chat: %v", err)
	}
}

// The text of a later assistant turn still starts its own message.
func TestDecodeResponsesAssistantTextAfterToolResults(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "list the docker dir"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "shell", "arguments": `{"command":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "a b c"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "The directory holds three files."},
			}},
		},
	}
	request, err := decodeBridgeRequest(Responses, input)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(request.Messages) != 4 {
		t.Fatalf("got %d bridge messages, want 4", len(request.Messages))
	}
	body, err := encodeBridgeRequest(Chat, request)
	if err != nil {
		t.Fatalf("encode chat: %v", err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := countOccurrences(string(encoded), `"tool_calls"`); got != 1 {
		t.Fatalf("tool_calls appears %d times, want 1: %s", got, encoded)
	}
}

func countOccurrences(haystack, needle string) int {
	count := 0
	for {
		index := indexOf(haystack, needle)
		if index < 0 {
			return count
		}
		count++
		haystack = haystack[index+len(needle):]
	}
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
