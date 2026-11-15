package protocol

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// adapterRenames is the rename set a shaped Codex request carries once the
// official tools are bound to exec_command and apply_patch: patch becomes a
// custom tool call, and read keeps the official name upstream while its
// arguments are rewritten into a synthesized command.
func adapterRenames() *ToolRenames {
	renames := NewToolRenames()
	renames.MarkCustom("apply_patch")
	renames.BindOfficial("patch", "apply_patch")
	renames.SetArgumentTransform("patch", func(string) (string, error) {
		return `{"input":"*** Begin Patch\n*** End Patch"}`, nil
	})
	renames.BindOfficial("read", "exec_command")
	renames.SetArgumentTransform("read", func(arguments string) (string, error) {
		if !strings.Contains(arguments, "a.txt") {
			return arguments, nil
		}
		return `{"cmd":"READ"}`, nil
	})
	return renames
}

func sseStream(frames ...string) io.Reader {
	var builder strings.Builder
	for _, frame := range frames {
		builder.WriteString("data: ")
		builder.WriteString(frame)
		builder.WriteString("\n\n")
	}
	builder.WriteString("data: [DONE]\n\n")
	return strings.NewReader(builder.String())
}

// TestTranscodeCustomToolCall checks a Chat upstream call to the official patch
// tool reaches a Responses client as a custom_tool_call with freeform input,
// streamed through the bridge (not the pass-through renamer).
func TestTranscodeCustomToolCall(t *testing.T) {
	upstream := sseStream(
		`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"patch","arguments":"{\"patchText\":\"*** Begin"}}]}}]}`,
		`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" Patch\\n*** End Patch\"}"}}]}}]}`,
		`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	recorder := httptest.NewRecorder()
	// The gateway renames the upstream stream before bridging protocols, so the
	// adapters run in the renamer and the emitter only relays.
	renamed := RenameStreamReader(upstream, Chat, adapterRenames())
	if _, _, err := TranscodeStream(context.Background(), recorder, renamed, Chat, Responses, "m", adapterRenames()); err != nil {
		t.Fatalf("TranscodeStream: %v", err)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `"function_call"`) {
		t.Fatalf("stream still carries a function_call item:\n%s", body)
	}
	if !strings.Contains(body, `"custom_tool_call"`) || !strings.Contains(body, `response.custom_tool_call_input.done`) {
		t.Fatalf("stream is missing the custom tool call events:\n%s", body)
	}
	if !strings.Contains(body, `"input":"*** Begin Patch\n*** End Patch"`) {
		t.Fatalf("custom tool call does not carry the patch text:\n%s", body)
	}
}

// TestTranscodeBuffersTransformedArguments checks a tool with an argument
// adapter streams one whole rewritten payload instead of the upstream JSON
// fragments, for both the Chat and Anthropic targets.
func TestTranscodeBuffersTransformedArguments(t *testing.T) {
	upstreamChat := func() io.Reader {
		return sseStream(
			`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
			`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
			`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}
	for _, target := range []Protocol{Chat, Anthropic, Responses} {
		recorder := httptest.NewRecorder()
		renamed := RenameStreamReader(upstreamChat(), Chat, adapterRenames())
		if _, _, err := TranscodeStream(context.Background(), recorder, renamed, Chat, target, "m", adapterRenames()); err != nil {
			t.Fatalf("%s: TranscodeStream: %v", target, err)
		}
		body := recorder.Body.String()
		if strings.Contains(body, `a.txt`) {
			t.Fatalf("%s: stream leaked the upstream arguments:\n%s", target, body)
		}
		if !strings.Contains(body, `READ`) {
			t.Fatalf("%s: stream is missing the rewritten arguments:\n%s", target, body)
		}
		switch target {
		case Chat, Anthropic:
			// One payload replaces the upstream fragments.
			if count := strings.Count(body, `READ`); count != 1 {
				t.Fatalf("%s: rewritten arguments written %d times:\n%s", target, count, body)
			}
		case Responses:
			// Every Responses event repeats the complete arguments, but no
			// event may still carry the upstream fragments.
			if strings.Contains(body, `"delta":"{\"path\":"`) {
				t.Fatalf("responses: stream still streams the upstream fragments:\n%s", body)
			}
		}
	}
}

// TestRenameStreamReaderTransformsResponsesArguments checks the Responses
// pass-through path: the argument deltas of a tool with an adapter are held
// back and the completed call carries the rewritten arguments.
func TestRenameStreamReaderTransformsResponsesArguments(t *testing.T) {
	upstream := sseStream(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"read"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"a.txt\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"path\":\"a.txt\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"path\":\"a.txt\"}","call_id":"call_1","name":"read"}}`,
	)
	out, err := io.ReadAll(RenameStreamReader(upstream, Responses, adapterRenames()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(out)
	if strings.Contains(body, `a.txt`) {
		t.Fatalf("stream leaked the unrewritten arguments:\n%s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"cmd\":\"READ\"}"`) {
		t.Fatalf("stream is missing the rewritten arguments:\n%s", body)
	}
	if strings.Contains(body, "function_call_arguments.delta") {
		t.Fatalf("stream still carries held-back argument deltas:\n%s", body)
	}
}

// TestRenameStreamReaderTransformsAnthropicArguments checks the Anthropic
// pass-through path: input_json_delta fragments are held back and the block
// closes with one rewritten delta followed by content_block_stop.
func TestRenameStreamReaderTransformsAnthropicArguments(t *testing.T) {
	upstream := sseStream(
		`{"type":"message_start","message":{"id":"msg_1","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	)
	out, err := io.ReadAll(RenameStreamReader(upstream, Anthropic, adapterRenames()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(out)
	if strings.Contains(body, `a.txt`) {
		t.Fatalf("stream leaked the unrewritten arguments:\n%s", body)
	}
	rewritten := strings.Index(body, `"partial_json":"{\"cmd\":\"READ\"}"`)
	stop := strings.Index(body, `"content_block_stop"`)
	if rewritten < 0 || stop < 0 || rewritten > stop {
		t.Fatalf("rewritten delta must precede content_block_stop:\n%s", body)
	}
	if count := strings.Count(body, "partial_json"); count != 1 {
		t.Fatalf("expected one rewritten delta, got %d:\n%s", count, body)
	}
}

// TestNonStreamingCustomToolCall checks the non-streaming Responses result of a
// Chat upstream call to the official patch tool is a custom_tool_call, which is
// what the client matches against its declared tool.
func TestNonStreamingCustomToolCall(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"patch","arguments":"{\"patchText\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`)
	renamed := RenameResponseTool(body, Chat, adapterRenames())
	out, err := ConvertResponse(Chat, Responses, renamed, adapterRenames())
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	output, _ := payload["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output items = %d: %s", len(output), out)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" {
		t.Fatalf("item type = %v, want custom_tool_call: %s", item["type"], out)
	}
	if item["name"] != "apply_patch" {
		t.Fatalf("item name = %v, want apply_patch", item["name"])
	}
	if item["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("item input = %v, want the freeform patch text", item["input"])
	}
}

// TestNonStreamingAnthropicToolTransform checks the non-streaming bridge to an
// Anthropic client carries the rewritten arguments in the tool_use input.
func TestNonStreamingAnthropicToolTransform(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`)
	renamed := RenameResponseTool(body, Chat, adapterRenames())
	out, err := ConvertResponse(Chat, Anthropic, renamed, adapterRenames())
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	content, _ := payload["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks = %d: %s", len(content), out)
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "exec_command" {
		t.Fatalf("block = %v: %s", block, out)
	}
	input, _ := block["input"].(map[string]any)
	if input["cmd"] != "READ" {
		t.Fatalf("tool_use input was not rewritten: %v", block["input"])
	}
}
