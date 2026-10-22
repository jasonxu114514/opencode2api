package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// An unknown model must be a 404 with a model_not_found code, so a client
// knows to pick a different model. The old code returned a 400 "the model
// uses an upstream protocol that opencode2api does not expose" for a bad
// model NAME as well, which made a model the upstream had retired (it answers
// 410 Gone) look like a gateway capability bug.
func TestUnknownModelReturns404ModelNotFound(t *testing.T) {
	gw := gatewayWithCatalog(t, []string{"known-model"})

	body := `{"model":"totally-unknown-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	telemetry.Middleware(telemetry.Monitor(), slog.New(slog.NewTextHandler(io.Discard, nil)), gw.Handler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown model (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Errorf("response does not carry the model_not_found code: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "totally-unknown-model") {
		t.Errorf("response does not name the missing model: %q", rec.Body.String())
	}
}

// A model the catalog KNOWS but cannot serve keeps the 400: that is a
// capability or routing problem, and the old phrasing is accurate for it.
func TestKnownButUnservableModelKeeps400(t *testing.T) {
	gw := gatewayWithCatalog(t, []string{"known-model"})
	// Mark the model as capability-unsupported on the Zen tier: the catalog
	// knows it, but it cannot be served.
	gw.catalog.ReplaceWithCapabilities(
		[]string{"known-model"}, nil,
		map[config.Tier]map[string]wire.Protocol{config.TierZen: {}, config.TierGo: {}},
		map[config.Tier]map[string]bool{config.TierZen: {"known-model": true}, config.TierGo: {}},
		map[config.Tier]map[string]modelcatalog.Metadata{config.TierZen: {}, config.TierGo: {}},
	)

	body := `{"model":"known-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	telemetry.Middleware(telemetry.Monitor(), slog.New(slog.NewTextHandler(io.Discard, nil)), gw.Handler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a known but unservable model (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream protocol") {
		t.Errorf("response lost the capability explanation: %q", rec.Body.String())
	}
}

// isPayloadTooLarge must only fire for a write-phase failure while the
// request budget is still alive.
func TestIsPayloadTooLargeClassification(t *testing.T) {
	writeTimeout := &phasedError{err: errAttemptTimeout, phase: "write"}
	headerTimeout := &phasedError{err: errAttemptTimeout, phase: "headers"}
	firstEventTimeout := &phasedError{err: errFirstEventTimeout, phase: "first_event"}
	dialTimeout := &phasedError{err: errAttemptTimeout, phase: "dial"}

	cases := []struct {
		name        string
		err         error
		budgetAlive bool
		want        bool
	}{
		{"write timeout with budget alive", writeTimeout, true, true},
		{"write timeout with budget expired", writeTimeout, false, false},
		{"header timeout", headerTimeout, true, false},
		{"first event timeout", firstEventTimeout, true, false},
		{"dial timeout", dialTimeout, true, false},
		{"nil error", nil, true, false},
		{"plain deadline", context.DeadlineExceeded, true, false},
	}
	for _, tc := range cases {
		if got := isPayloadTooLarge(tc.err, tc.budgetAlive); got != tc.want {
			t.Errorf("%s: isPayloadTooLarge = %v, want %v", tc.name, got, tc.want)
		}
	}
	if isPayloadTooLarge(errors.New("boom"), true) {
		t.Errorf("an unrelated error must not be classified as payload-too-large")
	}
}

// The client-facing status for an exhausted budget: 413 when the upstream
// never finished reading the body, 504 for every other phase.
func TestTimeoutStatusClassification(t *testing.T) {
	writeTimeout := &phasedError{err: errAttemptTimeout, phase: "write"}
	headerTimeout := &phasedError{err: errAttemptTimeout, phase: "headers"}
	firstEventTimeout := &phasedError{err: errFirstEventTimeout, phase: "first_event"}

	status, message, code := timeoutStatus(writeTimeout, true)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("write-phase timeout status = %d, want 413", status)
	}
	if code != "payload_too_large" {
		t.Errorf("write-phase timeout code = %q, want payload_too_large", code)
	}
	if !strings.Contains(message, "too large") {
		t.Errorf("write-phase timeout message = %q, want it to name the size problem", message)
	}

	// A header-phase failure is the upstream being slow: not the client's
	// fault, so it stays a 504.
	status, message, code = timeoutStatus(headerTimeout, true)
	if status != http.StatusGatewayTimeout {
		t.Errorf("header-phase timeout status = %d, want 504", status)
	}
	if code != "upstream_timeout" {
		t.Errorf("header-phase timeout code = %q, want upstream_timeout", code)
	}
	if message != "upstream request timed out" {
		t.Errorf("header-phase timeout message = %q", message)
	}

	// A first-event timeout is the model thinking: also a 504.
	if status, _, _ = timeoutStatus(firstEventTimeout, true); status != http.StatusGatewayTimeout {
		t.Errorf("first-event timeout status = %d, want 504", status)
	}

	// The request budget is what expired: the whole request was too slow,
	// which is not a payload problem.
	if status, _, _ = timeoutStatus(writeTimeout, false); status != http.StatusGatewayTimeout {
		t.Errorf("write timeout with an expired budget = %d, want 504", status)
	}
}

// gatewayWithCatalog builds a gateway whose catalog knows exactly the given
// models.
func gatewayWithCatalog(t *testing.T, models []string) *Gateway {
	t.Helper()
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		ZenKeys:    []string{"key-a"},
		Anonymous:  false,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 30},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		},
		Upstream: config.UpstreamConfig{Zen: "http://127.0.0.1:1"},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}
	gw.catalog.Replace(models, nil)
	return gw
}
