package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/telemetry"
)

// The request budget used to bound the whole downstream stream. A long
// thinking turn (xhigh/max reasoning, large context) legitimately runs for
// many minutes, and when the request budget expired mid-generation the
// upstream connection was torn down: the client saw a truncated stream and
// read the Go error text "context deadline exceeded" as a context-limit
// failure.
//
// These tests pin three properties:
//
//  1. With a separate stream budget, an established stream survives past the
//     request-level budget.
//  2. Without one, the historical behavior is unchanged (the request budget
//     still bounds the stream).
//  3. A client disconnect still tears the stream down immediately, because
//     the stream context derives from the client context.
func TestStreamBudgetLetsLongStreamOutliveRequestBudget(t *testing.T) {
	release := make(chan struct{})
	// releaseOnce keeps the upstream handler from blocking forever when the
	// test fails early: a failure must not hang the cleanup.
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Send one frame so the first-event gate is satisfied, then hold the
		// stream open past the request-level budget.
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry: config.RetryConfig{
			MaxAttempts: 2, TimeoutSeconds: 1, StreamTimeoutSeconds: 3,
		},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			AttemptTimeoutSeconds: 1, FirstEventTimeoutSeconds: 5,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	var raw string
	go func() {
		telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
		raw = rec.Body.String()
		close(done)
	}()

	// The request budget is 1s; the stream budget is 3s. After the request
	// budget has expired the stream must still be alive.
	select {
	case <-done:
		t.Fatalf("handler returned before the stream budget; the request budget still bounds the stream")
	case <-time.After(1500 * time.Millisecond):
	}

	releaseUpstream()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler did not return after the upstream released the stream")
	}
	if !strings.Contains(raw, "hi") {
		t.Fatalf("stream body missing the first frame: %q", raw)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Fatalf("stream was truncated: the request budget killed it mid-stream: %q", raw)
	}
}

// Without a stream budget the request-level budget still bounds the stream:
// the historical behavior must be preserved for operators who do not opt in.
func TestStreamBudgetAbsentKeepsRequestBudgetBound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Never finishes: the request budget must end the request.
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 2, TimeoutSeconds: 1},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			AttemptTimeoutSeconds: 1, FirstEventTimeoutSeconds: 5,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler never returned; the request budget no longer bounds the stream")
	}
}

// A client disconnect must tear the stream down immediately even when a
// separate stream budget is configured: the stream context is derived from the
// client context, not from the request context.
func TestStreamBudgetStillHonorsClientDisconnect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"r1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 2, TimeoutSeconds: 1, StreamTimeoutSeconds: 600},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			AttemptTimeoutSeconds: 1, FirstEventTimeoutSeconds: 5,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL, Anonymous: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	body := `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	// A request with a cancelable context stands in for a client that goes
	// away mid-stream.
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		telemetry.Middleware(monitor, logger, gw.Handler()).ServeHTTP(rec, req)
		close(done)
	}()

	// Let the stream start, then simulate the client going away.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("handler kept streaming after the client disconnected; the stream budget ignored the client context")
	}
}
