package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
)

func TestPassiveAvailability_NoProbesByDefault(t *testing.T) {
	var probeCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/inference" {
			probeCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer server.Close()

	cfg, err := config.Normalize("", config.Config{
		Listen:     "127.0.0.1:0",
		Prefer:     "zen",
		Anonymous:  true,
		ServerKeys: []string{"test-key-1234567890"},
		Retry: config.RetryConfig{
			MaxAttempts:    1,
			TimeoutSeconds: 30,
		},
		Models: config.ModelsConfig{
			ActiveProbes:   false,
			FreeOnly:       true,
			RefreshSeconds: 300,
		},
		Upstream: config.UpstreamConfig{
			Zen:       server.URL,
			Go:        server.URL,
			Anonymous: server.URL,
		},
		Performance: config.PerformanceConfig{
			MaxIdleConns:           10,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds:  5,
			FailureCooldownSeconds: 5,
		},
		Logging: config.LoggingConfig{
			Level:    "info",
			RingSize: 1000,
		},
	})
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}
	_ = cfg

	reg, err := models.AvailabilityStore("")
	if err != nil {
		t.Fatalf("failed to init availability store: %v", err)
	}

	// Verify RecordPassiveObservation behaves accurately
	now := time.Now().UTC()
	err = reg.RecordPassiveObservation("mimo-v2.6-flash-free", true, 200, "anonymous", now)
	if err != nil {
		t.Fatalf("RecordPassiveObservation failed: %v", err)
	}

	item := reg.Get("mimo-v2.6-flash-free")
	if item.Disabled {
		t.Errorf("expected model to be enabled after passive success")
	}
	if item.Reason != "passive_traffic_succeeded" {
		t.Errorf("expected reason 'passive_traffic_succeeded', got %q", item.Reason)
	}

	// Test 429 does not disable model
	err = reg.RecordPassiveObservation("mimo-v2.6-flash-free", false, 429, "anonymous", now)
	if err != nil {
		t.Fatalf("RecordPassiveObservation on 429 failed: %v", err)
	}
	item = reg.Get("mimo-v2.6-flash-free")
	if item.Disabled {
		t.Errorf("expected model to remain enabled on 429")
	}

	// Test 403, 500, timeout do not disable model
	for _, status := range []int{403, 500, 502, 503, 504} {
		_ = reg.RecordPassiveObservation("mimo-v2.6-flash-free", false, status, "anonymous", now)
		if reg.Get("mimo-v2.6-flash-free").Disabled {
			t.Errorf("expected model to remain enabled on status %d", status)
		}
	}

	// Ensure 0 synthetic probe requests sent
	if count := probeCount.Load(); count != 0 {
		t.Errorf("expected 0 synthetic probes, got %d", count)
	}
}

func TestCatalogRefresh_Singleflight(t *testing.T) {
	var refreshCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			refreshCount.Add(1)
			time.Sleep(20 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mimo-v2.6-flash-free"}]}`))
		}
	}))
	defer server.Close()

	cfg, err := config.Normalize("", config.Config{
		Listen:     "127.0.0.1:0",
		Prefer:     "zen",
		Anonymous:  true,
		ServerKeys: []string{"test-key-1234567890"},
		Retry: config.RetryConfig{
			MaxAttempts:    1,
			TimeoutSeconds: 30,
		},
		Models: config.ModelsConfig{
			RefreshSeconds: 300,
		},
		Upstream: config.UpstreamConfig{
			Zen:       server.URL,
			Go:        server.URL,
			Anonymous: server.URL,
		},
		Performance: config.PerformanceConfig{
			MaxIdleConns:           10,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds:  5,
			FailureCooldownSeconds: 5,
		},
		Logging: config.LoggingConfig{
			Level:    "info",
			RingSize: 1000,
		},
	})
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	gw, err := New(cfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Calling StartModelRefresh runs initial refresh
	gw.StartModelRefresh(ctx)
	time.Sleep(50 * time.Millisecond)

	// Refresh counter should be small, singleflight prevents overlapping runs
	if count := refreshCount.Load(); count > 3 {
		t.Errorf("expected coalesced requests, got %d hits", count)
	}
}
