package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestReasoningStreamPreservesWhitespace(t *testing.T) {
	for _, target := range []Protocol{Anthropic, Responses} {
		for _, field := range []string{"reasoning_content", "reasoning"} {
			t.Run(fmt.Sprint(target)+"/"+field, func(t *testing.T) {
				chunks := []string{"We", " need answer. ", "\n\n", "SELECT", " ", "date_tr", "unc('week', created_at)", "\t", "FROM profiles", "\u00a0", "中文 空格", "\n"}
				var input strings.Builder
				for _, part := range chunks {
					delta := map[string]any{field: part}
					if field == "reasoning_content" {
						delta["reasoning"] = "must not replace whitespace"
					} else {
						delta["reasoning_content"] = ""
					}
					b, _ := json.Marshal(map[string]any{"id": "sample", "model": "fixture", "choices": []any{map[string]any{"delta": delta}}})
					fmt.Fprintf(&input, "data: %s\n\n", b)
				}
				input.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"Done.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				w := httptest.NewRecorder()
				if _, _, err := TranscodeStream(context.Background(), w, strings.NewReader(input.String()), Chat, target, "fixture"); err != nil {
					t.Fatal(err)
				}
				var got []string
				err := readSSE(strings.NewReader(w.Body.String()), func(_ string, data string) error {
					var e struct {
						Type  string          `json:"type"`
						Delta json.RawMessage `json:"delta"`
					}
					if err := json.Unmarshal([]byte(data), &e); err != nil {
						return err
					}
					if e.Type == "content_block_delta" {
						var d struct {
							Type     string `json:"type"`
							Thinking string `json:"thinking"`
						}
						if err := json.Unmarshal(e.Delta, &d); err != nil {
							return err
						}
						if d.Type == "thinking_delta" {
							got = append(got, d.Thinking)
						}
					} else if e.Type == "response.reasoning_summary_text.delta" {
						var d string
						if err := json.Unmarshal(e.Delta, &d); err != nil {
							return err
						}
						got = append(got, d)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, chunks) {
					t.Fatalf("deltas changed:\ngot  %q\nwant %q", got, chunks)
				}
			})
		}
	}
}
