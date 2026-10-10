package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func toolImageRequest(t *testing.T) map[string]any {
	t.Helper()
	var request map[string]any
	err := json.Unmarshal([]byte(`{"model":"vision","messages":[
{"role":"assistant","content":[{"type":"tool_use","id":"A","name":"read","input":{}},{"type":"tool_use","id":"B","name":"read","input":{}}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"B","content":[{"type":"image","source":{"type":"url","url":"https://example.test/b.png"}}]}]},
{"role":"user","content":[{"type":"tool_result","tool_use_id":"A","content":[{"type":"text","text":"before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},{"type":"text","text":"after"},{"type":"image","source":{"type":"url","url":"https://example.test/a2.png"}}]},{"type":"text","text":"compare"},{"type":"image","source":{"type":"url","url":"https://example.test/user.png"}}]}]}`), &request)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestToolImagesChatOrderAndPurity(t *testing.T) {
	input := toolImageRequest(t)
	before, _ := json.Marshal(input)
	for _, from := range []Protocol{Anthropic, Responses} {
		source := input
		if from == Responses {
			var err error
			source, err = ConvertRequest(Anthropic, Responses, input)
			if err != nil {
				t.Fatal(err)
			}
		}
		for run := 0; run < 2; run++ {
			out, err := ConvertRequest(from, Chat, source)
			if err != nil {
				t.Fatal(err)
			}
			messages := out["messages"].([]any)
			if len(messages) != 6 {
				t.Fatalf("messages: %#v", messages)
			}
			for i, id := range []string{"A", "B"} {
				tool := messages[i+1].(map[string]any)
				if tool["role"] != "tool" || tool["tool_call_id"] != id {
					t.Fatalf("tool order: %#v", tool)
				}
				text := tool["content"].(string)
				if strings.Contains(text, "aW1hZ2U=") || !strings.Contains(text, `call_id="`+id+`"`) {
					t.Fatalf("tool content: %s", text)
				}
			}
			a := messages[3].(map[string]any)["content"].([]any)
			if len(a) != 4 || a[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,aW1hZ2U=" {
				t.Fatalf("attachments: %#v", a)
			}
			if !strings.Contains(a[2].(map[string]any)["text"].(string), "index=2") {
				t.Fatal("image index lost")
			}
			last, _ := json.Marshal(messages[5])
			if !strings.Contains(string(last), "user.png") {
				t.Fatal("user image lost")
			}
		}
	}
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
}

func TestToolImagesNativeAndPassThrough(t *testing.T) {
	input := toolImageRequest(t)
	r, err := ConvertRequest(Anthropic, Responses, input)
	if err != nil {
		t.Fatal(err)
	}
	items := r["input"].([]any)
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["type"] == "function_call_output" {
			if _, ok := item["output"].([]any); !ok {
				t.Fatal("tool image stringified")
			}
		}
	}
	a, err := ConvertRequest(Responses, Anthropic, r)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(a)
	if strings.Contains(string(encoded), "input_image") || !strings.Contains(string(encoded), `"type":"base64"`) {
		t.Fatalf("native image conversion: %s", encoded)
	}
	for _, tc := range []struct {
		p     Protocol
		input map[string]any
	}{{Anthropic, input}, {Responses, r}, {Chat, map[string]any{"messages": []any{map[string]any{"role": "tool", "content": "unchanged"}}}}} {
		out, err := ConvertRequest(tc.p, tc.p, tc.input)
		if err != nil {
			t.Fatal(err)
		}
		b1, _ := json.Marshal(out)
		b2, _ := json.Marshal(tc.input)
		if string(b1) != string(b2) {
			t.Fatal("passthrough changed")
		}
	}
}

func TestToolImagesRejectMalformedAndPreserveText(t *testing.T) {
	for _, value := range []any{nil, "", "plain", []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}} {
		out, err := decodeToolImages(Anthropic, value)
		if err != nil || !reflect.DeepEqual(out, value) {
			t.Fatal("legacy result changed")
		}
	}
	for _, tail := range []any{nil, map[string]any{"type": "document"}, map[string]any{"type": "tool_result"}, map[string]any{"type": "text"}, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "x", "data": "secret"}}} {
		value := []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.test/a"}}, tail}
		if _, err := decodeToolImages(Anthropic, value); err == nil {
			t.Fatalf("accepted malformed block: %#v", tail)
		}
	}
	for _, image := range []map[string]any{{"type": "input_image"}, {"type": "input_image", "file_id": "secret"}, {"type": "input_image", "image_url": "data:bad"}} {
		if _, err := decodeToolImages(Responses, []any{image}); err == nil {
			t.Fatal("accepted unsupported image")
		}
	}
	input := toolImageRequest(t)
	messages := input["messages"].([]any)
	input["messages"] = messages[:2]
	if _, err := ConvertRequest(Anthropic, Chat, input); err == nil {
		t.Fatal("accepted missing tool result")
	}
}

func TestToolImagesMultipleRounds(t *testing.T) {
	input := toolImageRequest(t)
	messages := input["messages"].([]any)
	// Reusing call IDs in a later, completed round must not retain attachments.
	input["messages"] = append(messages, messages...)
	out, err := ConvertRequest(Anthropic, Chat, input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(out)
	if strings.Count(string(encoded), "aW1hZ2U=") != 2 || len(out["messages"].([]any)) != 12 {
		t.Fatal("attachments leaked between rounds")
	}

	input = toolImageRequest(t)
	messages = input["messages"].([]any)
	input["messages"] = append(messages[:2], messages[1], messages[2])
	if _, err := ConvertRequest(Anthropic, Chat, input); err == nil {
		t.Fatal("duplicate result accepted")
	}
}
