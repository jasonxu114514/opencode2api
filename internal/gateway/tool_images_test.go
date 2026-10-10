package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// Exercise the real gateway handler and HTTP transport, not just converters.
func TestToolImagesForwardedHTTP(t *testing.T) {
	for _, from := range []wire.Protocol{wire.Anthropic, wire.Responses} {
		for _, to := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
			for _, stream := range []bool{false, true} {
				t.Run(string(from)+"-"+string(to)+map[bool]string{false: "-json", true: "-stream"}[stream], func(t *testing.T) {
					captured := make(chan map[string]any, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != wire.Path(to) {
							t.Errorf("upstream path %s", r.URL.Path)
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						captured <- body
						// Also prove a supplier rejection is surfaced, never retried without images.
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(400)
						io.WriteString(w, `{"error":{"message":"fixture rejects image","type":"invalid_request_error"}}`)
					}))
					defer upstream.Close()
					path := filepath.Join(t.TempDir(), "config.json")
					cfgJSON := map[string]any{"server_keys": []string{"local-test"}, "go_keys": []string{"upstream-test"}, "upstream": map[string]string{"go": upstream.URL, "zen": upstream.URL}, "models": map[string]any{"protocols": map[string]string{"vision": string(to)}}}
					b, _ := json.Marshal(cfgJSON)
					if err := os.WriteFile(path, b, 0600); err != nil {
						t.Fatal(err)
					}
					cfg, err := config.Load(path)
					if err != nil {
						t.Fatal(err)
					}
					g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewMonitor())
					if err != nil {
						t.Fatal(err)
					}
					g.catalog.Replace(nil, []string{"vision"})
					server := httptest.NewServer(g.Handler())
					defer server.Close()
					var input map[string]any
					json.Unmarshal([]byte(`{"model":"vision","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"A","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"A","content":[{"type":"text","text":"read result"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}]}`), &input)
					if from == wire.Responses {
						input, err = wire.ConvertRequest(wire.Anthropic, wire.Responses, input)
						if err != nil {
							t.Fatal(err)
						}
					}
					input["stream"] = stream
					b, _ = json.Marshal(input)
					req, _ := http.NewRequest("POST", server.URL+wire.Path(from), bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer local-test")
					resp, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					response, _ := io.ReadAll(resp.Body)
					if resp.StatusCode != 400 || !strings.Contains(string(response), "fixture rejects image") {
						t.Fatalf("status=%d body=%s", resp.StatusCode, response)
					}
					select {
					case body := <-captured:
						encoded, _ := json.Marshal(body)
						if bytes.Contains(encoded, []byte(`\"source\"`)) || bytes.Contains(encoded, []byte(`\"input_image\"`)) {
							t.Fatal("image stringified")
						}
						if strings.Count(string(encoded), "aW1hZ2U=") != 1 {
							t.Fatalf("image lost or duplicated: %s", encoded)
						}
						if body["stream"] != stream {
							t.Fatal("stream flag changed")
						}
						if to == wire.Chat {
							messages := body["messages"].([]any)
							tool := messages[1].(map[string]any)
							if strings.Contains(tool["content"].(string), "aW1hZ2U=") {
								t.Fatal("base64 in tool text")
							}
						}
					default:
						t.Fatal("no upstream request")
					}
					if from != to {
						var removeSources func(any)
						removeSources = func(v any) {
							switch x := v.(type) {
							case map[string]any:
								if x["type"] == "image" {
									delete(x, "source")
								}
								if x["type"] == "input_image" {
									delete(x, "image_url")
								}
								for _, child := range x {
									removeSources(child)
								}
							case []any:
								for _, child := range x {
									removeSources(child)
								}
							}
						}
						removeSources(input)
						b, _ = json.Marshal(input)
						req, _ = http.NewRequest("POST", server.URL+wire.Path(from), bytes.NewReader(b))
						req.Header.Set("Authorization", "Bearer local-test")
						bad, err := server.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						io.Copy(io.Discard, bad.Body)
						bad.Body.Close()
						if bad.StatusCode != 400 {
							t.Fatalf("invalid image status: %d", bad.StatusCode)
						}
						select {
						case <-captured:
							t.Fatal("invalid image reached upstream")
						default:
						}
					}
				})
			}
		}
	}
}
