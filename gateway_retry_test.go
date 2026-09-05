package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsStaleReasoningReference(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":{"message":"Upstream request failed: [invalid_request_error] Referenced reasoning item 'rs_7145a5ca29c1537d081efdb1' was not found or has expired.", "type":"invalid_request_error"}}`, true},
		{`{"error":{"message":"reasoning item rs_abc has expired", "type":"invalid_request_error"}}`, true},
		{`{"error":{"message":"Reasoning reference rs_xyz does not exist", "type":"invalid_request_error"}}`, true},
		// Negative cases: generic errors that merely mention reasoning must not match.
		{`{"error":{"message":"Model is unavailable.", "type":"upstream_error"}}`, false},
		{`{"error":{"message":"unknown reasoning field 'foo'", "type":"invalid_request_error"}}`, false},
		{`{"error":{"message":"invalid reasoning_effort value 'high'", "type":"invalid_request_error"}}`, false},
		{`{"error":{"message":"request body must be a JSON object", "type":"invalid_request_error"}}`, false},
		{`not json at all`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isStaleReasoningReference([]byte(tc.body)); got != tc.want {
			t.Errorf("isStaleReasoningReference(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestStripStaleReasoningInputs(t *testing.T) {
	route := modelRoute{
		ID:        "test-model",
		Tier:      TierZen,
		Protocol:  ProtocolResponses,
		Protocols: map[Tier]Protocol{TierZen: ProtocolResponses, TierGo: ProtocolChat},
	}
	responsesBody := []byte(`{"model":"test-model","previous_response_id":"resp_123","input":[{"type":"reasoning","id":"rs_old","summary":[]},{"type":"message","role":"user","content":"hi"}]}`)
	chatBody := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	bodies := map[Tier][]byte{TierZen: responsesBody, TierGo: chatBody}

	stripped, changed := stripStaleReasoningInputs(route, bodies)
	if !changed {
		t.Fatal("expected changed=true when reasoning references exist")
	}
	var payload map[string]any
	if err := json.Unmarshal(stripped[TierZen], &payload); err != nil {
		t.Fatalf("stripped zen body is not valid JSON: %v", err)
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Error("previous_response_id was not removed")
	}
	input, _ := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("expected 1 input item left, got %d", len(input))
	}
	if m, _ := input[0].(map[string]any); stringAt(m, "type") != "message" {
		t.Errorf("unexpected remaining input item: %v", input[0])
	}
	if string(stripped[TierGo]) != string(chatBody) {
		t.Error("non-Responses tier body must pass through untouched")
	}

	cleanBodies := map[Tier][]byte{TierZen: []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`)}
	if _, changed := stripStaleReasoningInputs(route, cleanBodies); changed {
		t.Error("expected changed=false when no reasoning references exist")
	}
}

const retryTestStaleError = `{"error":{"message":"Upstream request failed: [invalid_request_error] Referenced reasoning item 'rs_old' was not found or has expired.", "type":"invalid_request_error"}}`

func retryTestUpstreamBody() []byte {
	return []byte(`{"model":"test-model","previous_response_id":"resp_123","input":[{"type":"reasoning","id":"rs_old","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"message","role":"user","content":"hi"}]}`)
}

func newRetryTestGateway(t *testing.T, upstreamURL string) *Gateway {
	t.Helper()
	cfg := Config{
		ServerKeys:  []string{"test-key"},
		Anonymous:   true,
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: upstreamURL, Go: upstreamURL},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 30},
		Performance: PerformanceConfig{MaxIdleConns: 32, MaxIdleConnsPerHost: 8, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Prefer:      TierZen,
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Logging:     LoggingConfig{Level: "error", RingSize: 100},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g, err := NewGateway(cfg, logger, NewMonitor())
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func retryTestRoute() modelRoute {
	return modelRoute{
		ID:        "test-model",
		Tier:      TierZen,
		Protocol:  ProtocolResponses,
		Protocols: map[Tier]Protocol{TierZen: ProtocolResponses},
		Anonymous: true,
	}
}

func retryTestIDs() requestIDs {
	return requestIDs{Session: "ses_test", Request: "req_test", Project: "prj_test"}
}

func upstreamInputHasReasoning(t *testing.T, raw []byte) bool {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("upstream request is not valid JSON: %v", err)
	}
	if _, ok := payload["previous_response_id"]; ok {
		return true
	}
	for _, item := range sliceAt(payload, "input") {
		if m, ok := item.(map[string]any); ok && stringAt(m, "type") == "reasoning" {
			return true
		}
	}
	return false
}

func TestDoUpstreamRetriesStaleReasoningSuccess(t *testing.T) {
	var hits atomic.Int64
	var secondBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(retryTestStaleError))
			return
		}
		secondBody.Store(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer server.Close()

	g := newRetryTestGateway(t, server.URL)
	resp, _, err := g.doUpstream(context.Background(), retryTestRoute(), map[Tier][]byte{TierZen: retryTestUpstreamBody()}, retryTestIDs())
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", hits.Load())
	}
	raw, _ := secondBody.Load().([]byte)
	if upstreamInputHasReasoning(t, raw) {
		t.Errorf("retry request still carries reasoning references: %s", raw)
	}
}

func TestDoUpstreamRetryNon2xxRestoresOriginal(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(retryTestStaleError))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom", "type":"server_error"}}`))
	}))
	defer server.Close()

	g := newRetryTestGateway(t, server.URL)
	resp, _, err := g.doUpstream(context.Background(), retryTestRoute(), map[Tier][]byte{TierZen: retryTestUpstreamBody()}, retryTestIDs())
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected original 400, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != retryTestStaleError {
		t.Errorf("expected original error body restored, got %q", body)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", hits.Load())
	}
}

func TestDoUpstreamNonMatching400PassesThrough(t *testing.T) {
	var hits atomic.Int64
	const unavailable = `{"error":{"message":"Model is unavailable.", "type":"upstream_error"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(unavailable))
	}))
	defer server.Close()

	g := newRetryTestGateway(t, server.URL)
	resp, _, err := g.doUpstream(context.Background(), retryTestRoute(), map[Tier][]byte{TierZen: retryTestUpstreamBody()}, retryTestIDs())
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	defer drainAndClose(resp.Body)
	if hits.Load() != 1 {
		t.Fatalf("expected no retry, got %d upstream hits", hits.Load())
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != unavailable {
		t.Errorf("expected passthrough body, got %q", body)
	}
}

func TestDoUpstreamStaleErrorWithoutReasoningNoRetry(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(retryTestStaleError))
	}))
	defer server.Close()

	g := newRetryTestGateway(t, server.URL)
	clean := []byte(`{"model":"test-model","input":[{"type":"message","role":"user","content":"hi"}]}`)
	resp, _, err := g.doUpstream(context.Background(), retryTestRoute(), map[Tier][]byte{TierZen: clean}, retryTestIDs())
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	defer drainAndClose(resp.Body)
	if hits.Load() != 1 {
		t.Fatalf("expected no retry when payload has nothing to strip, got %d hits", hits.Load())
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDoUpstreamRetryAttemptsAreCumulative(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(retryTestStaleError))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer server.Close()

	monitor := NewMonitor()
	cfg := Config{
		ServerKeys:  []string{"test-key"},
		Anonymous:   true,
		Proxies:     []string{"direct"},
		Upstream:    UpstreamConfig{Zen: server.URL, Go: server.URL},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 30},
		Performance: PerformanceConfig{MaxIdleConns: 32, MaxIdleConnsPerHost: 8, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 1},
		Prefer:      TierZen,
		Models:      ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Logging:     LoggingConfig{Level: "error", RingSize: 100},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), monitor)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	resp, _, err := g.doUpstream(context.Background(), retryTestRoute(), map[Tier][]byte{TierZen: retryTestUpstreamBody()}, retryTestIDs())
	if err != nil {
		t.Fatalf("doUpstream: %v", err)
	}
	drainAndClose(resp.Body)
	var numbers []int
	for _, attempt := range monitor.Snapshot().Upstream.Recent {
		if attempt.RequestID == "req_test" {
			numbers = append(numbers, attempt.Attempt)
		}
	}
	if len(numbers) != 2 || numbers[0] != 1 || numbers[1] != 2 {
		t.Errorf("expected cumulative attempt numbers [1 2], got %v", numbers)
	}
}
