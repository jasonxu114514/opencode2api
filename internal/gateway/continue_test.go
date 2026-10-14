package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

func coreToolset() []any {
	names := []string{"bash", "edit", "glob", "grep", "read"}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Agent tool " + name,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return tools
}

func baseZenBody(t *testing.T, messages []any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":            "test-model",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
		"reasoning_effort": "high",
		"tools":            coreToolset(),
		"messages":         messages,
	})
	if err != nil {
		t.Fatalf("marshal base body: %v", err)
	}
	return body
}

func decodeContinuation(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal continuation body: %v", err)
	}
	return payload
}

func continuationMessages(t *testing.T, payload map[string]any) []any {
	t.Helper()
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("continuation body has no messages: %v", payload)
	}
	return messages
}

func messageField(t *testing.T, message any, key string) any {
	t.Helper()
	entry, ok := message.(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %T", message)
	}
	return entry[key]
}

func TestBuildContinuationBody_trailingUser(t *testing.T) {
	base := baseZenBody(t, []any{map[string]any{"role": "user", "content": "hi"}})
	out, err := buildContinuationBody(base, "partial text")
	if err != nil {
		t.Fatalf("buildContinuationBody: %v", err)
	}
	payload := decodeContinuation(t, out)
	messages := continuationMessages(t, payload)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if got := messageField(t, messages[1], "role"); got != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", got)
	}
	if got := messageField(t, messages[1], "content"); got != "partial text" {
		t.Fatalf("messages[1].content = %v, want partial text", got)
	}
	if got := messageField(t, messages[2], "role"); got != "user" {
		t.Fatalf("messages[2].role = %v, want user", got)
	}
	if got := messageField(t, messages[2], "content"); got != continuationInstruction {
		t.Fatalf("messages[2].content = %v, want the continuation instruction", got)
	}
	if got := payload["stream"]; got != true {
		t.Fatalf("stream = %v, want true", got)
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not preserved: %v", payload["stream_options"])
	}
	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 5 {
		t.Fatalf("tools not preserved: %v", payload["tools"])
	}
}

func TestBuildContinuationBody_trailingAssistantMerged(t *testing.T) {
	base := baseZenBody(t, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "X"},
	})
	out, err := buildContinuationBody(base, "partial")
	if err != nil {
		t.Fatalf("buildContinuationBody: %v", err)
	}
	payload := decodeContinuation(t, out)
	messages := continuationMessages(t, payload)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if got := messageField(t, messages[1], "role"); got != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", got)
	}
	if got := messageField(t, messages[1], "content"); got != "Xpartial" {
		t.Fatalf("messages[1].content = %v, want Xpartial", got)
	}
	if got := messageField(t, messages[2], "content"); got != continuationInstruction {
		t.Fatalf("messages[2].content = %v, want the continuation instruction", got)
	}
	assistants := 0
	for _, message := range messages {
		if messageField(t, message, "role") == "assistant" {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant message count = %d, want exactly 1 before the instruction", assistants)
	}
}

func TestResumeSegment_rejectsBadPartial(t *testing.T) {
	resumer := &gatewayStreamResumer{}
	if _, err := resumer.ResumeSegment(""); err == nil {
		t.Fatal("empty partial must be rejected")
	}
	if _, err := resumer.ResumeSegment(strings.Repeat("a", maxContinuationPartialBytes+1)); err == nil {
		t.Fatal("oversized partial must be rejected")
	}
}

func TestStreamResumeLimit_defaultsToOne(t *testing.T) {
	g := &Gateway{cfg: config.Config{Performance: config.PerformanceConfig{StreamContinuationMax: 0}}}
	if got := g.streamResumeLimit(); got != 1 {
		t.Fatalf("streamResumeLimit() = %d for 0, want 1", got)
	}
	g.cfg.Performance.StreamContinuationMax = 3
	if got := g.streamResumeLimit(); got != 3 {
		t.Fatalf("streamResumeLimit() = %d for 3, want 3", got)
	}
}

func TestResumeSegmentUsesClientContextAfterInitialBudgetExpires(t *testing.T) {
	requestCanceled := make(chan struct{}, 2)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"continued"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		requestCanceled <- struct{}{}
	}))
	defer server.Close()

	performance := config.PerformanceConfig{
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, ConnectTimeoutSeconds: 1,
		IdleConnTimeoutSeconds: 10, FailureCooldownSeconds: 30,
		FirstEventTimeoutSeconds: 2, StreamContinuationTimeoutSeconds: 2,
	}
	transports, err := newTransportPool([]string{"direct", "direct"}, performance, time.Second)
	if err != nil {
		t.Fatalf("newTransportPool: %v", err)
	}
	pool := newAnonymousPool(true, transports, 30*time.Second)
	ids := identity.RequestIDs{Session: "ses_test", Request: "req_test"}
	startNode := pool.CursorFor(ids.Session).start
	state := &anonymousStreamState{node: pool.nodes[startNode]}
	meta := &telemetry.RequestMeta{}
	clientCtx := telemetry.WithRequestMeta(context.Background(), meta)
	initialCtx, cancelInitial := context.WithCancel(clientCtx)
	cancelInitial()
	if initialCtx.Err() == nil {
		t.Fatal("initial request context is not expired")
	}

	g := &Gateway{
		cfg: config.Config{
			Anonymous:   true,
			Upstream:    config.UpstreamConfig{Zen: server.URL},
			Performance: performance,
		},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		anonymous: pool,
	}
	resumer := &gatewayStreamResumer{
		g: g, clientCtx: clientCtx, streamState: state,
		continuationTimeout: 2 * time.Second,
		route:               models.Route{ID: "test-model", Tier: config.TierZen, Protocol: wire.Chat},
		ids:                 ids, zenBody: baseZenBody(t, []any{map[string]any{"role": "user", "content": "hello"}}), meta: meta,
	}
	reader, err := resumer.ResumeSegment("partial answer")
	if err != nil {
		t.Fatalf("ResumeSegment with expired initial context: %v", err)
	}
	closer, ok := reader.(io.Closer)
	if !ok {
		t.Fatalf("resumed reader %T is not closable", reader)
	}
	buf := make([]byte, 1)
	if n, err := reader.Read(buf); n != 1 || err != nil {
		t.Fatalf("read resumed stream first byte: n=%d err=%v", n, err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close resumed stream: %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("closing continuation body did not cancel upstream request")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
	if state.node == pool.nodes[startNode] {
		t.Fatal("continuation reused the exit that was just marked failed")
	}
	if meta.ResumeAttempts != 1 || meta.ResumeSuccesses != 1 || meta.Continued != 1 {
		t.Fatalf("resume telemetry = attempts:%d successes:%d continued:%d", meta.ResumeAttempts, meta.ResumeSuccesses, meta.Continued)
	}
}

func TestResumeSegmentTimeoutCancelsBeforeFirstEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	performance := config.PerformanceConfig{
		MaxIdleConns: 1, MaxIdleConnsPerHost: 1, ConnectTimeoutSeconds: 1,
		IdleConnTimeoutSeconds: 10, FailureCooldownSeconds: 1,
		FirstEventTimeoutSeconds: 0, StreamContinuationTimeoutSeconds: 50,
	}
	transports, err := newTransportPool([]string{"direct"}, performance, 0)
	if err != nil {
		t.Fatalf("newTransportPool: %v", err)
	}
	g := &Gateway{
		cfg:       config.Config{Anonymous: true, Upstream: config.UpstreamConfig{Zen: server.URL}, Performance: performance},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		anonymous: newAnonymousPool(true, transports, time.Second),
	}
	meta := &telemetry.RequestMeta{}
	resumer := &gatewayStreamResumer{
		g: g, clientCtx: context.Background(), continuationTimeout: 50 * time.Millisecond,
		route:   models.Route{ID: "test-model", Tier: config.TierZen, Protocol: wire.Chat},
		ids:     identity.RequestIDs{Session: "ses_timeout", Request: "req_timeout"},
		zenBody: baseZenBody(t, []any{map[string]any{"role": "user", "content": "hello"}}), meta: meta,
	}
	_, err = resumer.ResumeSegment("partial answer")
	if !errors.Is(err, errContinuationTimeout) {
		t.Fatalf("ResumeSegment error = %v, want errContinuationTimeout", err)
	}
	if meta.ResumeAttempts != 1 || meta.ResumeFailures != 1 {
		t.Fatalf("resume telemetry = attempts:%d failures:%d", meta.ResumeAttempts, meta.ResumeFailures)
	}
}

func TestResumeSegmentStopsBudgetAfterFirstEvent(t *testing.T) {
	firstEvent := make(chan struct{})
	release := make(chan struct{})
	requestCanceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(firstEvent)
		select {
		case <-release:
		case <-r.Context().Done():
			requestCanceled <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, `data: [DONE]`+"\n\n")
	}))
	defer server.Close()

	performance := config.PerformanceConfig{
		MaxIdleConns: 1, MaxIdleConnsPerHost: 1, ConnectTimeoutSeconds: 1,
		IdleConnTimeoutSeconds: 10, FailureCooldownSeconds: 1,
		FirstEventTimeoutSeconds: 1, StreamContinuationTimeoutSeconds: 1,
	}
	transports, err := newTransportPool([]string{"direct"}, performance, 0)
	if err != nil {
		t.Fatalf("newTransportPool: %v", err)
	}
	meta := &telemetry.RequestMeta{}
	g := &Gateway{
		cfg:       config.Config{Anonymous: true, Upstream: config.UpstreamConfig{Zen: server.URL}, Performance: performance},
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		anonymous: newAnonymousPool(true, transports, time.Second),
	}
	resumer := &gatewayStreamResumer{
		g: g, clientCtx: context.Background(), continuationTimeout: 500 * time.Millisecond,
		route:   models.Route{ID: "test-model", Tier: config.TierZen, Protocol: wire.Chat},
		ids:     identity.RequestIDs{Session: "ses_long", Request: "req_long"},
		zenBody: baseZenBody(t, []any{map[string]any{"role": "user", "content": "hello"}}), meta: meta,
	}
	reader, err := resumer.ResumeSegment("partial answer")
	if err != nil {
		t.Fatalf("ResumeSegment: %v", err)
	}
	closer := reader.(io.Closer)
	defer closer.Close()
	select {
	case <-firstEvent:
	case <-time.After(time.Second):
		t.Fatal("upstream did not send first event")
	}
	select {
	case <-requestCanceled:
		t.Fatal("continuation budget canceled an established stream")
	case <-time.After(600 * time.Millisecond):
	}
	close(release)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read resumed stream after first event: %v", err)
	}
	if !strings.Contains(string(data), "[DONE]") {
		t.Fatalf("resumed stream lost terminal event: %q", data)
	}
}

func TestResumeSegmentClientCancellationSkipsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	meta := &telemetry.RequestMeta{}
	resumer := &gatewayStreamResumer{
		clientCtx: ctx, continuationTimeout: time.Second, meta: meta,
	}
	if _, err := resumer.ResumeSegment("partial answer"); err == nil {
		t.Fatal("ResumeSegment after client cancellation returned nil error")
	}
	if meta.ResumeSkipped != "client_canceled" || meta.ResumeAttempts != 0 {
		t.Fatalf("resume telemetry = skipped:%q attempts:%d", meta.ResumeSkipped, meta.ResumeAttempts)
	}
}

func TestResumeSegmentDisabledBudgetSkipsUpstream(t *testing.T) {
	meta := &telemetry.RequestMeta{}
	resumer := &gatewayStreamResumer{
		clientCtx: context.Background(), meta: meta,
	}
	if _, err := resumer.ResumeSegment("partial answer"); err == nil {
		t.Fatal("ResumeSegment with a disabled budget returned nil error")
	}
	if meta.ResumeSkipped != "budget_disabled" || meta.ResumeAttempts != 0 {
		t.Fatalf("resume telemetry = skipped:%q attempts:%d", meta.ResumeSkipped, meta.ResumeAttempts)
	}
}
