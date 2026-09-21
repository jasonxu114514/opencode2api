package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func rotationGateway(t *testing.T, models []string, fn http.HandlerFunc) *Gateway {
	t.Helper()
	up := httptest.NewServer(fn)
	t.Cleanup(up.Close)
	cfg := config.Config{ServerKeys: []string{"local"}, GoKeys: []string{"go-key-one", "go-key-two"}, Prefer: config.TierGo, Upstream: config.UpstreamConfig{Go: up.URL}, Retry: config.RetryConfig{MaxAttempts: 99, TimeoutSeconds: 15}, Models: config.ModelsConfig{RefreshSeconds: 300}, Performance: config.PerformanceConfig{ConnectTimeoutSeconds: 1}}
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	native := map[string]wire.Protocol{}
	for _, model := range models {
		native[model] = wire.Chat
	}
	g.catalog.ReplaceWithCapabilities(nil, models, map[config.Tier]map[string]wire.Protocol{config.TierGo: native}, nil, nil)
	t.Cleanup(func() {
		for _, p := range g.transports.items {
			p.client.CloseIdleConnections()
		}
	})
	return g
}
func modelBody(r *http.Request) map[string]any {
	var p map[string]any
	_ = json.NewDecoder(r.Body).Decode(&p)
	return p
}
func replyChat(w http.ResponseWriter, model string, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"answer\",\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"complete answer\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n", model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"answer","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"complete answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, model)
}
func callRotation(g *Gateway, model string, stream bool) (*httptest.ResponseRecorder, *telemetry.RequestMeta) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, model, stream)))
	r.Header.Set("Authorization", "Bearer local")
	meta := &telemetry.RequestMeta{}
	r = r.WithContext(telemetry.WithRequestMeta(r.Context(), meta))
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	return w, meta
}
func TestRotationBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status func(string, int) int
		want   int
		code   int
	}{
		{"ordinary three models times three", func(string, int) int { return 500 }, 9, 503},
		{"quota skips more than three", func(m string, _ int) int {
			if m == "f" {
				return 200
			}
			return 429
		}, 6, 200},
		{"whole group quota", func(string, int) int { return 429 }, 6, 503},
		{"mixed quota keeps ordinary budget", func(_ string, n int) int {
			if n == 2 {
				return 429
			}
			return 500
		}, 8, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			per := map[string]int{}
			g := rotationGateway(t, []string{"a", "b", "c", "d", "e", "f"}, func(w http.ResponseWriter, r *http.Request) {
				p := modelBody(r)
				m := p["model"].(string)
				calls = append(calls, m)
				per[m]++
				status := tc.status(m, len(calls))
				if status == 200 {
					replyChat(w, m, false)
					return
				}
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"failed"}}`)
			})
			w, _ := callRotation(g, "sweet", false)
			if len(calls) != tc.want || w.Code != tc.code {
				t.Fatalf("calls=%v status=%d body=%s", calls, w.Code, w.Body)
			}
			for _, node := range g.goNodes.nodes {
				if node.cooldownUntil.Load() != 0 {
					t.Fatal("model failure cooled entire key")
				}
			}
			if tc.name == "whole group quota" {
				for _, until := range g.rotation.Snapshot().Groups[1].Paused {
					if time.Until(until) < 119*time.Second {
						t.Fatal("Retry-After ignored")
					}
				}
			}
		})
	}
}
func TestRotationSuccessAliasesAndDirectModels(t *testing.T) {
	calls := []string{}
	g := rotationGateway(t, []string{"glm-5.3-flash", "deepseek-v4.1-flash", "glm-5.3"}, func(w http.ResponseWriter, r *http.Request) {
		p := modelBody(r)
		m := p["model"].(string)
		calls = append(calls, m)
		replyChat(w, m, p["stream"] == true)
	})
	for _, alias := range []string{"sota", "sweet"} {
		for _, stream := range []bool{false, true, false} {
			w, meta := callRotation(g, alias, stream)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"`+alias+`"`) || w.Header().Get("X-Resolved-Model") == "" {
				t.Fatalf("%s: %s", alias, w.Body)
			}
			if meta.Model == alias || meta.ModelAlias != alias || !meta.UsageReported || meta.Usage.Output != 3 {
				t.Fatalf("incorrect real-model usage: %+v", meta)
			}
		}
	}
	if !slices.Equal(calls, []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash", "deepseek-v4.1-flash", "glm-5.3-flash", "glm-5.3-flash", "glm-5.3-flash"}) {
		t.Fatal(calls)
	}
	w, _ := callRotation(g, "glm-5.3", false)
	if !strings.Contains(w.Body.String(), `"model":"glm-5.3"`) || w.Header().Get("X-Resolved-Model") != "" {
		t.Fatal("direct route changed")
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer local")
	w = httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	for _, alias := range []string{"sota", "sweet"} {
		if !strings.Contains(w.Body.String(), `"id":"`+alias+`"`) {
			t.Fatal("alias missing")
		}
	}
}
func TestRotationStreamingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, first string
		wantFirst   int
		partial     bool
	}{
		{"error before content", "data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"rate limit\"}}\n\n", 2, false},
		{"invalid prelude", "data: invalid\n\n", 4, false},
		{"partial then error", "data: {\"id\":\"x\",\"model\":\"a\",\"choices\":[{\"delta\":{\"content\":\"partial original\"}}]}\n\ndata: {\"error\":{\"message\":\"provider failed\"}}\n\n", 1, true},
		{"partial then eof", "data: {\"id\":\"x\",\"model\":\"a\",\"choices\":[{\"delta\":{\"content\":\"partial original\"}}]}\n\n", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
				p := modelBody(r)
				m := p["model"].(string)
				calls = append(calls, m)
				w.Header().Set("Content-Type", "text/event-stream")
				if m == "a" {
					fmt.Fprint(w, tc.first)
					return
				}
				replyChat(w, m, true)
			})
			w, _ := callRotation(g, "sweet", true)
			if len(calls) != tc.wantFirst {
				t.Fatal(calls)
			}
			if tc.partial {
				if strings.Contains(w.Body.String(), "complete answer") || !strings.Contains(w.Body.String(), "partial original") || !strings.Contains(w.Body.String(), "upstream_error") {
					t.Fatal(w.Body)
				}
				if g.rotation.Snapshot().Groups[1].Current != "b" {
					t.Fatal("next request was not advanced")
				}
				w, _ = callRotation(g, "sweet", true)
				if calls[len(calls)-1] != "b" || !strings.Contains(w.Body.String(), "complete answer") {
					t.Fatal(calls, w.Body)
				}
			} else if !strings.Contains(w.Body.String(), "complete answer") || strings.Contains(w.Body.String(), "upstream_error") {
				t.Fatal(w.Body)
			}
		})
	}
}
func TestRotationMalformedResponseAndCrossProtocol(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
				p := modelBody(r)
				calls++
				if p["model"] == "a" {
					fmt.Fprint(w, `{"unexpected":"schema"}`)
					return
				}
				if r.URL.Path != "/v1/messages" {
					t.Errorf("protocol not reconverted: %s", r.URL.Path)
				}
				fmt.Fprint(w, `{"id":"x","type":"message","role":"assistant","model":"b","content":[{"type":"text","text":"anthropic answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`)
			})
			g.catalog.ReplaceWithCapabilities(nil, []string{"a", "b"}, map[config.Tier]map[string]wire.Protocol{config.TierGo: {"a": wire.Chat, "b": wire.Anthropic}}, nil, nil)
			input := `{"model":"sweet","messages":[{"role":"user","content":"hello"}],"max_tokens":50}`
			if endpoint == "/v1/responses" {
				input = `{"model":"sweet","input":"hello"}`
			}
			r := httptest.NewRequest("POST", endpoint, strings.NewReader(input))
			r.Header.Set("Authorization", "Bearer local")
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, r)
			if calls != 4 || w.Code != 200 || !strings.Contains(w.Body.String(), "anthropic answer") || !strings.Contains(w.Body.String(), `"model":"sweet"`) {
				t.Fatal(calls, w.Code, w.Body)
			}
		})
	}
}
func TestRotationFirstOutputTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout before content", func(t *testing.T) {
		var mu sync.Mutex
		calls := []string{}
		g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
			m := modelBody(r)["model"].(string)
			mu.Lock()
			calls = append(calls, m)
			mu.Unlock()
			if m == "a" {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			replyChat(w, m, true)
		})
		cfg := g.cfg.Rotation
		cfg.FirstOutputSeconds = 1
		g.rotation.Configure(cfg)
		w, _ := callRotation(g, "sweet", true)
		if w.Code != 200 || w.Header().Get("X-Resolved-Model") != "b" {
			t.Fatal(w.Code, w.Body)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(calls, []string{"a", "a", "a", "b"}) {
			t.Fatal(calls)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			<-r.Context().Done()
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"sweet","messages":[],"stream":true}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer local")
		done := make(chan struct{})
		go func() { g.Handler().ServeHTTP(httptest.NewRecorder(), r); close(done) }()
		<-entered
		cancel()
		<-done
		state := g.rotation.Snapshot().Groups[1]
		if state.Current != "a" || len(state.Paused) != 0 {
			t.Fatal("cancel rotated", state)
		}
	})
}

func TestRotationStreamingCrossProtocol(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			calls := 0
			g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
				p := modelBody(r)
				calls++
				if p["model"] == "a" {
					w.WriteHeader(429)
					return
				}
				if r.URL.Path != "/v1/messages" {
					t.Errorf("wrong protocol %s", r.URL.Path)
				}
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"x\",\"model\":\"b\",\"usage\":{\"input_tokens\":2}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"full answer\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			})
			g.catalog.ReplaceWithCapabilities(nil, []string{"a", "b"}, map[config.Tier]map[string]wire.Protocol{config.TierGo: {"a": wire.Chat, "b": wire.Anthropic}}, nil, nil)
			input := `{"model":"sweet","stream":true,"messages":[{"role":"user","content":"hello"}],"max_tokens":50}`
			if endpoint == "/v1/responses" {
				input = `{"model":"sweet","stream":true,"input":"hello"}`
			}
			r := httptest.NewRequest("POST", endpoint, strings.NewReader(input))
			r.Header.Set("Authorization", "Bearer local")
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, r)
			if calls != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), "full answer") || !strings.Contains(w.Body.String(), `"model":"sweet"`) || strings.Contains(w.Body.String(), "upstream_error") {
				t.Fatal(calls, w.Code, w.Body)
			}
		})
	}
}

type signalWriter struct {
	*httptest.ResponseRecorder
	once  sync.Once
	first chan struct{}
}

func (w *signalWriter) Write(b []byte) (int, error) {
	n, e := w.ResponseRecorder.Write(b)
	w.once.Do(func() { close(w.first) })
	return n, e
}
func TestRotationInflightPinnedAndCancelAfterOutput(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelClient), func(t *testing.T) {
			resume := make(chan struct{})
			g := rotationGateway(t, []string{"a", "b"}, func(w http.ResponseWriter, r *http.Request) {
				modelBody(r)
				fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"a\",\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-resume:
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" last\"}}]}\n\ndata: [DONE]\n\n")
				case <-r.Context().Done():
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"sweet","stream":true,"messages":[]}`)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer local")
			w := &signalWriter{ResponseRecorder: httptest.NewRecorder(), first: make(chan struct{})}
			done := make(chan struct{})
			go func() { g.Handler().ServeHTTP(w, r); close(done) }()
			<-w.first
			if cancelClient {
				cancel()
			} else {
				if e := g.rotation.Manual("sweet", "b"); e != nil {
					t.Fatal(e)
				}
				close(resume)
			}
			<-done
			s := g.rotation.Snapshot().Groups[1]
			if cancelClient {
				if s.Current != "a" || len(s.Paused) > 0 {
					t.Fatal("cancel after output rotated")
				}
			} else if s.Current != "b" || !strings.Contains(w.Body.String(), " last") || w.Header().Get("X-Resolved-Model") != "a" {
				t.Fatal("inflight stream interrupted or cursor reverted", s, w.Body)
			}
		})
	}
}
