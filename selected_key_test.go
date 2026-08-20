package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func selectedKeyTestConfig(upstream string) Config {
	return Config{
		ServerKeys: []string{"local-key"},
		ZenKeys:    []string{"bad-key", "good-key"},
		Proxies:    []string{"direct"},
		Upstream:   UpstreamConfig{Zen: upstream, Go: upstream},
		Retry:      RetryConfig{MaxAttempts: 3, TimeoutSeconds: 5},
		Models:     ModelsConfig{RefreshSeconds: 300, Protocols: map[string]string{}},
		Performance: PerformanceConfig{
			MaxIdleConns: 10, MaxIdleConnsPerHost: 10, IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1,
		},
		Prefer: TierZen,
	}
}

func TestSelectedKeyDoesNotRotate(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer bad-key" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway, err := NewGateway(selectedKeyTestConfig(server.URL), logger, NewMonitor())
	if err != nil {
		t.Fatal(err)
	}
	route := modelRoute{ID: "test-model", Tier: TierZen, Protocol: ProtocolChat, KeyTiers: []Tier{TierZen}}
	override := debugKeyOverride{Tier: TierZen, KeyID: secretFingerprint("bad-key")}
	resp, err := gateway.doSelectedKeyUpstream(context.Background(), route, []byte(`{"model":"test-model"}`), requestIDs{Request: "req-test"}, override)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestSelectedKeyUsesRequestedCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer good-key" {
			t.Fatalf("unexpected authorization header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway, err := NewGateway(selectedKeyTestConfig(server.URL), logger, NewMonitor())
	if err != nil {
		t.Fatal(err)
	}
	route := modelRoute{ID: "test-model", Tier: TierZen, Protocol: ProtocolChat, KeyTiers: []Tier{TierZen}}
	override := debugKeyOverride{Tier: TierZen, KeyID: secretFingerprint("good-key")}
	resp, err := gateway.doSelectedKeyUpstream(context.Background(), route, []byte(`{"model":"test-model"}`), requestIDs{Request: "req-test"}, override)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestRouteForTierRejectsUnavailableModel(t *testing.T) {
	catalog := newModelCatalog(TierGo, nil)
	catalog.Replace([]string{"zen-model"}, []string{"go-model"})
	_, err := catalog.RouteForTier("go-model", ProtocolChat, TierZen, true, true)
	if err == nil || !strings.Contains(err.Error(), "selected zen key tier") {
		t.Fatalf("unexpected error: %v", err)
	}
}
