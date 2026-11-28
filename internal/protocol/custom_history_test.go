package protocol

import (
	"encoding/json"
	"testing"
)

// customHistory is the shape Codex replays after auto-compaction: the custom
// apply_patch call and its output, as Responses items.
func customHistory() map[string]any {
	return map[string]any{
		"model":  "m",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "create hello.txt"}}},
			map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "apply_patch", "input": "*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "Success. Updated the following files:\nA hello.txt"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "thanks"}}},
		},
	}
}

// TestConvertRequestCustomToolHistory checks a replayed custom tool call and its
// output survive the Responses bridge instead of failing the request.
func TestConvertRequestCustomToolHistory(t *testing.T) {
	for _, target := range []Protocol{Chat, Anthropic} {
		converted, err := ConvertRequest(Responses, target, customHistory())
		if err != nil {
			t.Fatalf("%s: ConvertRequest: %v", target, err)
		}
		switch target {
		case Chat:
			messages, _ := converted["messages"].([]any)
			toolCall, toolResult := false, false
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if calls, ok := message["tool_calls"].([]any); ok && len(calls) == 1 {
					call, _ := calls[0].(map[string]any)
					function, _ := call["function"].(map[string]any)
					if function["name"] != "apply_patch" {
						t.Fatalf("Chat: tool call name = %v, want apply_patch", function["name"])
					}
					var arguments map[string]any
					if err := json.Unmarshal([]byte(function["arguments"].(string)), &arguments); err != nil {
						t.Fatalf("Chat: tool arguments are not JSON: %v", err)
					}
					if arguments["input"] != "*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch" {
						t.Fatalf("Chat: freeform input was lost: %v", arguments["input"])
					}
					toolCall = true
				}
				if message["role"] == "tool" && message["tool_call_id"] == "call_1" {
					if message["content"] != "Success. Updated the following files:\nA hello.txt" {
						t.Fatalf("Chat: tool output = %v", message["content"])
					}
					toolResult = true
				}
			}
			if !toolCall || !toolResult {
				t.Fatalf("Chat: call %v result %v in %s", toolCall, toolResult, mustJSON(converted))
			}
		case Anthropic:
			messages, _ := converted["messages"].([]any)
			toolUse, toolResult := false, false
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				blocks, _ := message["content"].([]any)
				for _, rawBlock := range blocks {
					block, _ := rawBlock.(map[string]any)
					switch block["type"] {
					case "tool_use":
						if block["name"] != "apply_patch" || block["id"] != "call_1" {
							t.Fatalf("Anthropic: tool_use = %v", block)
						}
						input, _ := block["input"].(map[string]any)
						if input["input"] == nil {
							t.Fatalf("Anthropic: freeform input was lost: %v", block["input"])
						}
						toolUse = true
					case "tool_result":
						if block["tool_use_id"] != "call_1" {
							t.Fatalf("Anthropic: tool_result = %v", block)
						}
						toolResult = true
					}
				}
			}
			if !toolUse || !toolResult {
				t.Fatalf("Anthropic: call %v result %v in %s", toolUse, toolResult, mustJSON(converted))
			}
		}
	}
}

// mustJSON renders a payload for failure messages.
func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unencodable>"
	}
	return string(encoded)
}
