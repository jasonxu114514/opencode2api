package gateway

import (
	"context"
	"encoding/json"
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

// The upstream per-key model listings are not equal. Four of six configured
// keys listed 85 models, two listed 44, and the 44-model set was a strict
// subset of the 85-model one (a free-tier account sees a smaller catalog than
// a paid one). The catalog refresh adopted ONE key's list, so the catalog
// oscillated between 44 and 85 models on every refresh and every model missing
// from the adopted list became a client-visible 400.
//
// The tier listing must be the union over the keys: a model is servable when
// any configured key can serve it.
func TestRefreshTierUnionsModelListsAcrossKeys(t *testing.T) {
	// The upstream answers a different listing per key.
	var mu sync.Mutex
	listings := map[string][]string{
		"key-a": {"model-a", "model-b"},
		"key-b": {"model-b", "model-c"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		mu.Lock()
		models, ok := listings[key]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		data := make([]map[string]any, 0, len(models))
		for _, id := range models {
			data = append(data, map[string]any{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		ZenKeys:    []string{"key-a", "key-b"},
		Anonymous:  false,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 6, TimeoutSeconds: 30},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := gw.refreshTier(ctx, upstream.URL, gw.zenNodes)

	mu.Lock()
	defer mu.Unlock()
	union := map[string]bool{}
	for _, models := range listings {
		for _, m := range models {
			union[m] = true
		}
	}
	if len(got) != len(union) {
		t.Fatalf("refreshTier returned %d models (%v), want the union of %d", len(got), got, len(union))
	}
	for model := range union {
		found := false
		for _, m := range got {
			if m == model {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("model %q missing from the merged listing %v", model, got)
		}
	}
}

// A tier where every key fails must return nil so the previous catalog stays
// in place instead of being wiped.
func TestRefreshTierAllKeysFailReturnsNil(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		ZenKeys:    []string{"key-a", "key-b"},
		Anonymous:  false,
		Prefer:     config.TierZen,
		Retry:      config.RetryConfig{MaxAttempts: 6, TimeoutSeconds: 30},
		Performance: config.PerformanceConfig{
			ConnectTimeoutSeconds: 2, IdleConnTimeoutSeconds: 30,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		},
		Upstream: config.UpstreamConfig{Zen: upstream.URL},
		Models:   config.ModelsConfig{RefreshSeconds: 300},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	monitor := telemetry.Monitor()
	gw, err := New(cfg, logger, monitor)
	if err != nil {
		t.Fatalf("gateway New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if got := gw.refreshTier(ctx, upstream.URL, gw.zenNodes); got != nil {
		t.Fatalf("refreshTier returned %v when every key failed; nil must keep the previous catalog", got)
	}
}
