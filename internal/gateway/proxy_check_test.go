package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

const modelCheckStream = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func TestProxyCheckRequiresRealModelReply(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, code string
		status                        int
		ok                            bool
	}{
		{"text stream", modelCheckStream, "text/event-stream", "", 200, true},
		{"json reply", `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`, "application/json", "", 200, true},
		{"trace is not inference", "ip=203.0.113.42\n", "text/plain", "invalid_response", 200, false},
		{"blocked", `{"error":{"type":"FreeTierError","message":"Free usage denied for this IP"}}`, "application/json", "free_tier_denied", 403, false},
		{"limited", `{"error":{"message":"Too many requests"}}`, "application/json", "rate_limited", 429, false},
		{"proxy auth", "authentication required", "text/plain", "proxy_auth", 407, false},
		{"retired", `{"error":{"message":"model_not_found"}}`, "application/json", "model_unavailable", 404, false},
		{"html", "<html>sign in</html>", "text/html", "invalid_response", 200, false},
		{"empty reply", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, "application/json", "invalid_response", 200, false},
		{"reasoning only", `{"choices":[{"message":{"reasoning_content":"Thinking"},"finish_reason":"length"}]}`, "application/json", "invalid_response", 200, false},
		{"empty choices", `{"choices":[]}`, "application/json", "invalid_response", 200, false},
		{"error envelope", `{"error":{"type":"FreeTierError","message":"Denied"}}`, "application/json", "invalid_response", 200, false},
		{"incomplete stream", "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n", "text/event-stream", "invalid_response", 200, false},
		{"empty completed stream", "data: [DONE]\n\n", "text/event-stream", "invalid_response", 200, false},
		{"stream error", "data: {\"error\":{\"message\":\"Rate limit reached\"}}\n\n", "text/event-stream", "invalid_response", 200, false},
		{"redirect", "", "text/plain", "upstream_error", 302, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer public" {
					t.Error("probe must use anonymous credentials")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["model"] != "glm-5.3-flash-free" || payload["stream"] != true || payload["max_tokens"] != float64(1024) || payload["tools"] == nil {
					t.Errorf("probe does not use anonymous model request")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			result := checkProxyModel(context.Background(), server.Client(), []string{server.URL}, "glm-5.3-flash-free", wire.Chat, time.Second, time.Second)
			if result.OK != tc.ok || result.ErrorCode != tc.code || result.HTTPStatus != tc.status || result.Model != "glm-5.3-flash-free" || len(result.Attempts) != 1 {
				t.Fatalf("unexpected check: %+v", result)
			}
			if tc.ok && result.Output != "OK" {
				t.Fatalf("missing actual model output: %+v", result)
			}
			if !tc.ok && result.Error == "" {
				t.Fatal("failure has no reason")
			}
			if tc.status == 403 && !strings.Contains(result.Error, "FreeTierError") {
				t.Fatalf("upstream failure reason lost: %+v", result)
			}
		})
	}
}

func TestProxyModelCheckFallbackKeepsAttemptDetails(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/zen/") {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"type":"FreeTierError","message":"denied"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, modelCheckStream)
	}))
	defer server.Close()
	result := checkProxyModel(context.Background(), server.Client(), []string{server.URL + "/zen", server.URL + "/inference"}, "glm-5.3-flash-free", wire.Chat, time.Second, time.Second)
	if !result.OK || result.Output != "OK" || calls.Load() != 2 || len(result.Attempts) != 2 || result.Attempts[0].ErrorCode != "free_tier_denied" || result.Error != "" {
		t.Fatalf("fallback result: %+v", result)
	}
}

func TestProxyCheckTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		result := checkProxyModel(ctx, server.Client(), []string{server.URL}, "glm-5.3-flash-free", wire.Chat, 30*time.Millisecond, 30*time.Millisecond)
		cancel()
		if result.OK || result.ErrorCode != "timeout" || result.Error == "" {
			t.Fatalf("unexpected canceled/timeout check: %+v", result)
		}
	}
}

func TestManualProxyCheckUsesCredentialsWithoutChangingLiveHealth(t *testing.T) {
	const username, password = "check-user", "check-password"
	var auth atomic.Value
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Proxy-Authorization"))
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	u.User = url.UserPassword(username, password)
	cfg := config.Config{Proxies: []string{u.String()}, Performance: config.PerformanceConfig{ConnectTimeoutSeconds: 1}, Upstream: config.UpstreamConfig{Anonymous: "https://fallback.invalid/inference"}}
	live := &proxyTransport{name: u.String()}
	node := &anonymousNode{proxy: live}
	node.cooldownUntil.Store(42)
	catalog := models.NewCatalog(config.TierZen, map[string]string{"glm-5.3-flash-free": "chat"})
	catalog.Replace([]string{"glm-5.3-flash-free"}, nil)
	manager := &RuntimeManager{}
	manager.current.Store(&gatewayRuntime{config: cfg, gateway: &Gateway{cfg: cfg, catalog: catalog, transports: &transportPool{items: []*proxyTransport{live}}, anonymous: &anonymousPool{nodes: []*anonymousNode{node}}}})
	result, err := manager.CheckProxy(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || live.healthy.Load() || live.checking.Load() || node.cooldownUntil.Load() != 42 || calls.Load() != 2 {
		t.Fatalf("check changed state, escaped proxy, or accepted rejected proxy: %+v", result)
	}
	if auth.Load() != "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)) {
		t.Fatal("proxy authentication was not forwarded")
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), username) || strings.Contains(string(data), password) {
		t.Fatal("proxy credentials leaked")
	}
}

func TestProxyCheckWithoutFreeModelIsManagementError(t *testing.T) {
	for _, available := range []bool{false, true} {
		manager := &RuntimeManager{}
		if available {
			catalog := models.NewCatalog(config.TierZen, nil)
			catalog.Replace([]string{"paid-model"}, nil)
			manager.current.Store(&gatewayRuntime{gateway: &Gateway{catalog: catalog}})
		}
		if _, err := manager.CheckProxy(context.Background(), "direct"); err == nil {
			t.Fatal("missing catalog/free model should not mark proxy unavailable")
		}
	}
}

func TestProxyCheckRedactsUnsavedEncodedCredentials(t *testing.T) {
	u, _ := url.Parse("http://proxy.example:8080")
	u.User = url.UserPassword("draft-user", "p@ss word")
	got := redactProxyCheckError("failed "+u.String()+" password=p@ss word", "pool+"+u.String())
	for _, secret := range []string{"draft-user", "p@ss word", "p%40ss%20word"} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential %q leaked: %s", secret, got)
		}
	}
}

func TestProxyCheckNativeProtocols(t *testing.T) {
	for _, tc := range []struct {
		protocol wire.Protocol
		body     string
	}{
		{wire.Anthropic, `{"type":"message","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`},
		{wire.Responses, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wire.Path(tc.protocol) {
					t.Errorf("wrong native path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			result := checkProxyModel(context.Background(), server.Client(), []string{server.URL}, "example-free", tc.protocol, time.Second, time.Second)
			if !result.OK || result.Output != "OK" {
				t.Fatalf("native reply rejected: %+v", result)
			}
		})
	}
}
