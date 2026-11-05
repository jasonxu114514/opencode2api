package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestPassiveAvailability_RuntimeControllerFlow(t *testing.T) {
	var inferenceRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/inference" {
			inferenceRequests.Add(1)
		} else if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mimo-v2.6-flash-free"}]}`))
			return
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
			ActiveProbes:   false, // Default: passive only
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := RuntimeManager(ctx, "", cfg, logger, nil, nil, config.SecretRedactor(), level)
	if err != nil {
		t.Fatalf("RuntimeManager failed: %v", err)
	}
	defer mgr.Shutdown()

	// Execute checkFreeModels directly through controller
	mgr.checkFreeModels(ctx)

	// In default false state, inferenceRequests must be strictly 0
	if count := inferenceRequests.Load(); count != 0 {
		t.Errorf("expected 0 inference requests with ActiveProbes=false, got %d", count)
	}
}

func TestActiveProbes_ExplicitOptIn(t *testing.T) {
	var probeCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/inference" {
			probeCount.Add(1)
		} else if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mimo-v2.6-flash-free"}]}`))
			return
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
			ActiveProbes:   true, // Explicitly opted in
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := RuntimeManager(ctx, "", cfg, logger, nil, nil, config.SecretRedactor(), level)
	if err != nil {
		t.Fatalf("RuntimeManager failed: %v", err)
	}
	defer mgr.Shutdown()

	// Ensure model is registered in catalog and availability
	runtime := mgr.current.Load()
	native := map[config.Tier]map[string]wire.Protocol{
		config.TierZen: {"mimo-v2.6-flash-free": wire.Chat},
	}
	runtime.gateway.catalog.ReplaceWithCapabilities([]string{"mimo-v2.6-flash-free"}, nil, native, nil, nil)
	_ = runtime.availability.ScheduleSweep()

	// Execute checkFreeModels
	mgr.checkFreeModels(ctx)

	// Active probes should have fired at least 1 probe request
	if count := probeCount.Load(); count == 0 {
		t.Errorf("expected active probes to run when ActiveProbes=true, got 0 requests")
	}
}

func TestRecordPassiveObservation_Robustness(t *testing.T) {
	reg, err := models.AvailabilityStore("")
	if err != nil {
		t.Fatalf("AvailabilityStore failed: %v", err)
	}
	now := time.Now().UTC()

	// 1. Success enables model
	_ = reg.RecordPassiveObservation("test-model", true, 200, "", "anonymous", now)
	if reg.Get("test-model").Disabled {
		t.Errorf("expected model to be enabled on 200")
	}

	// 2. Generic errors MUST NOT disable model
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"invalid request parameters"}`},
		{404, `{"error":"path not found"}`},
		{429, `{"error":"rate limit exceeded"}`},
		{403, `{"error":"forbidden"}`},
		{500, `{"error":"internal server error"}`},
		{502, `{"error":"bad gateway"}`},
		{503, `{"error":"service unavailable"}`},
	} {
		_ = reg.RecordPassiveObservation("test-model", false, tc.status, tc.body, "anonymous", now)
		if reg.Get("test-model").Disabled {
			t.Errorf("expected status %d with body %q to NOT disable model", tc.status, tc.body)
		}
	}

	// 3. Authoritative model_unavailable disables model
	authoritativeBody := `{"error":{"code":"model_not_found","message":"The model test-model does not exist"}}`
	_ = reg.RecordPassiveObservation("test-model", false, 404, authoritativeBody, "anonymous", now)
	if !reg.Get("test-model").Disabled {
		t.Errorf("expected authoritative model_not_found to disable model")
	}
	if reg.Get("test-model").Reason != "model_unavailable" {
		t.Errorf("expected reason 'model_unavailable', got %q", reg.Get("test-model").Reason)
	}
}

func TestCatalogRefresh_PreservesLastKnownGood(t *testing.T) {
	var shouldFail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if shouldFail.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "server error")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"stable-model-free"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(cfg, logger, nil, nil)
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}

	// 1. Initial successful refresh with capabilities
	zenModels := gw.refreshZen(context.Background())
	if len(zenModels) == 0 || zenModels[0] != "stable-model-free" {
		t.Fatalf("expected initial model list to contain stable-model-free, got %v", zenModels)
	}
	native := map[config.Tier]map[string]wire.Protocol{
		config.TierZen: {"stable-model-free": wire.Chat},
	}
	gw.catalog.ReplaceWithCapabilities(zenModels, nil, native, nil, nil)

	// Verify catalog currently lists the model
	if current := gw.catalog.List(); len(current) == 0 || current[0] != "stable-model-free" {
		t.Fatalf("expected catalog to have stable-model-free initially, got %v", current)
	}

	// 2. Upstream starts failing on next refresh round
	shouldFail.Store(true)
	failedZenModels := gw.refreshZen(context.Background())
	if failedZenModels != nil {
		t.Errorf("expected refreshZen to return nil on failure, got %v", failedZenModels)
	}

	// Only replace if at least one listing succeeded (as implemented in StartModelRefresh)
	if failedZenModels != nil {
		gw.catalog.ReplaceWithCapabilities(failedZenModels, nil, nil, nil, nil)
	}

	// Catalog should still preserve last-known-good models
	current := gw.catalog.List()
	if len(current) == 0 || current[0] != "stable-model-free" {
		t.Errorf("expected last-known-good catalog to be preserved, got %v", current)
	}
}

func TestCatalogRefresh_OverlapProtection(t *testing.T) {
	var refreshCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			refreshCount.Add(1)
			time.Sleep(50 * time.Millisecond)
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := New(cfg, logger, nil, nil)
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}

	// Trigger multiple concurrent RefreshCatalog calls
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gw.RefreshCatalog(context.Background())
		}()
	}
	wg.Wait()

	// Due to TryLock overlap guard on Gateway, overlapping runs were skipped
	if count := refreshCount.Load(); count != 1 {
		t.Errorf("expected exactly 1 refresh execution due to overlap guard, got %d", count)
	}
}
