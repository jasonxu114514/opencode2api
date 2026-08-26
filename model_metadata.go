package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	modelsDevDefaultURL = "https://models.dev/api.json"
	modelsDevRefresh    = 24 * time.Hour
	modelsDevTimeout    = 30 * time.Second
)

type ModelPrice struct {
	ID         string   `json:"id"`
	Input      *float64 `json:"input_cost,omitempty"`
	Output     *float64 `json:"output_cost,omitempty"`
	Deprecated bool     `json:"deprecated"`
}

type AnonymousDecision struct {
	Allowed    bool     `json:"allowed"`
	Source     string   `json:"source"`
	Known      bool     `json:"known"`
	Deprecated bool     `json:"deprecated"`
	InputCost  *float64 `json:"input_cost,omitempty"`
	OutputCost *float64 `json:"output_cost,omitempty"`
}

type MetadataSnapshot struct {
	Ready       bool       `json:"ready"`
	Models      int        `json:"models"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	Stale       bool       `json:"stale"`
	LastError   string     `json:"last_error,omitempty"`
	CachePath   string     `json:"-"`
	NextRefresh *time.Time `json:"next_refresh,omitempty"`
}

type modelMetadataCache struct {
	UpdatedAt time.Time             `json:"updated_at"`
	Models    map[string]ModelPrice `json:"models"`
}

type modelMetadataStore struct {
	mu             sync.RWMutex
	models         map[string]ModelPrice
	updatedAt      time.Time
	lastError      string
	cachePath      string
	endpoint       string
	client         *http.Client
	clientProvider func() []*http.Client
	logger         *slog.Logger
}

func newModelMetadataStore(configPath string, logger *slog.Logger) *modelMetadataStore {
	cachePath := ""
	if configPath != "" {
		cachePath = configPath + ".models.dev.json"
	}
	store := &modelMetadataStore{
		models: make(map[string]ModelPrice), cachePath: cachePath, endpoint: modelsDevDefaultURL,
		client: &http.Client{Timeout: modelsDevTimeout}, logger: logger,
	}
	if err := store.loadCache(); err != nil && !errors.Is(err, os.ErrNotExist) {
		store.lastError = "load metadata cache: " + err.Error()
	}
	return store
}

// SetClientProvider supplies candidate HTTP clients for refresh attempts, most
// preferred first. It lets the periodic models.dev refresh ride the proxy
// transports exposed by whichever gateway runtime is currently active.
func (store *modelMetadataStore) SetClientProvider(provider func() []*http.Client) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.clientProvider = provider
}

func (store *modelMetadataStore) refreshClients() []*http.Client {
	store.mu.RLock()
	provider := store.clientProvider
	store.mu.RUnlock()
	if provider == nil {
		return []*http.Client{store.client}
	}
	clients := provider()
	if len(clients) == 0 {
		return []*http.Client{store.client}
	}
	return clients
}

func (store *modelMetadataStore) Start(ctx context.Context) {
	go func() {
		store.refreshAndLog(ctx)
		ticker := time.NewTicker(modelsDevRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				store.refreshAndLog(ctx)
			}
		}
	}()
}

func (store *modelMetadataStore) refreshAndLog(ctx context.Context) {
	if err := store.Refresh(ctx); err != nil {
		if store.logger != nil {
			store.logger.Warn("models.dev metadata refresh failed", "component", "models", "event", "metadata_refresh_failed", "error", err)
		}
		return
	}
	if store.logger != nil {
		store.logger.Info("models.dev metadata refreshed", "component", "models", "event", "metadata_refreshed", "models", store.Snapshot().Models)
	}
}

func (store *modelMetadataStore) Refresh(ctx context.Context) error {
	var lastErr error
	for _, client := range store.refreshClients() {
		data, err := store.fetch(ctx, client)
		if err != nil {
			lastErr = err
			continue
		}
		models, err := decodeModelsDev(data)
		if err != nil {
			return store.recordError(err)
		}
		now := time.Now().UTC()
		cache := modelMetadataCache{UpdatedAt: now, Models: models}
		if store.cachePath != "" {
			if err := saveMetadataCache(store.cachePath, cache); err != nil {
				return store.recordError(err)
			}
		}
		store.mu.Lock()
		store.models, store.updatedAt, store.lastError = models, now, ""
		store.mu.Unlock()
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("no HTTP client available for models.dev refresh")
	}
	return store.recordError(lastErr)
}

func (store *modelMetadataStore) fetch(ctx context.Context, client *http.Client) ([]byte, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, modelsDevTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(refreshCtx, http.MethodGet, store.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", opencodeUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("models.dev returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func (store *modelMetadataStore) recordError(err error) error {
	store.mu.Lock()
	store.lastError = err.Error()
	store.mu.Unlock()
	return err
}

func (store *modelMetadataStore) Decide(model string) AnonymousDecision {
	store.mu.RLock()
	price, exists := store.models[model]
	ready := !store.updatedAt.IsZero() && len(store.models) > 0
	store.mu.RUnlock()
	nameFree := isFreeModel(model)
	fallback := func(source string) AnonymousDecision {
		if nameFree {
			return AnonymousDecision{Allowed: true, Source: "name_free", Known: false}
		}
		return AnonymousDecision{Allowed: false, Source: source, Known: false}
	}
	if !ready {
		return fallback("metadata_pending")
	}
	if !exists {
		return fallback("metadata_model_missing")
	}
	decision := AnonymousDecision{
		Known: true, Deprecated: price.Deprecated, InputCost: price.Input, OutputCost: price.Output,
	}
	metadataFree := !price.Deprecated && price.Input != nil && price.Output != nil && *price.Input == 0 && *price.Output == 0
	if nameFree || metadataFree {
		decision.Allowed = true
		switch {
		case nameFree && metadataFree:
			decision.Source = "name_and_metadata_free"
		case nameFree:
			decision.Source = "name_free"
		default:
			decision.Source = "metadata_free"
		}
		return decision
	}
	if price.Deprecated {
		decision.Source = "metadata_deprecated"
		return decision
	}
	if price.Input == nil || price.Output == nil {
		decision.Known = false
		decision.Source = "metadata_cost_unknown"
		return decision
	}
	decision.Source = "metadata_paid"
	return decision
}

func (store *modelMetadataStore) Price(model string) (ModelPrice, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	price, ok := store.models[model]
	return price, ok
}

func (store *modelMetadataStore) Snapshot() MetadataSnapshot {
	store.mu.RLock()
	defer store.mu.RUnlock()
	snapshot := MetadataSnapshot{Ready: !store.updatedAt.IsZero() && len(store.models) > 0, Models: len(store.models), LastError: store.lastError, CachePath: store.cachePath}
	if !store.updatedAt.IsZero() {
		updated := store.updatedAt.UTC()
		next := updated.Add(modelsDevRefresh)
		snapshot.UpdatedAt, snapshot.NextRefresh = &updated, &next
		snapshot.Stale = time.Since(updated) > modelsDevRefresh
	}
	return snapshot
}

func (store *modelMetadataStore) loadCache() error {
	if store.cachePath == "" {
		return nil
	}
	file, err := os.Open(store.cachePath)
	if err != nil {
		return err
	}
	defer file.Close()
	var cache modelMetadataCache
	decoder := json.NewDecoder(io.LimitReader(file, 32<<20))
	if err := decoder.Decode(&cache); err != nil {
		return err
	}
	if cache.UpdatedAt.IsZero() || len(cache.Models) == 0 {
		return errors.New("metadata cache is empty or missing updated_at")
	}
	store.models, store.updatedAt = cache.Models, cache.UpdatedAt.UTC()
	return nil
}

func decodeModelsDev(data []byte) (map[string]ModelPrice, error) {
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("decode models.dev: %w", err)
	}
	keys := make([]string, 0, len(providers))
	for key := range providers {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left, right := metadataProviderRank(keys[i]), metadataProviderRank(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	for _, key := range keys {
		if metadataProviderRank(key) > 1 {
			continue
		}
		var provider map[string]any
		if json.Unmarshal(providers[key], &provider) != nil {
			continue
		}
		if metadataProviderRank(key) == 1 {
			identity := strings.ToLower(firstString(stringAt(provider, "id"), stringAt(provider, "name")))
			if !strings.Contains(identity, "opencode") {
				continue
			}
		}
		models := mapAt(provider, "models")
		if len(models) == 0 {
			continue
		}
		result := make(map[string]ModelPrice, len(models))
		for id, raw := range models {
			model, _ := raw.(map[string]any)
			modelID := firstString(stringAt(model, "id"), id)
			cost := mapAt(model, "cost")
			result[modelID] = ModelPrice{
				ID: modelID, Input: numberPointer(cost, "input"), Output: numberPointer(cost, "output"), Deprecated: metadataDeprecated(model),
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	return nil, errors.New("models.dev contains no OpenCode model metadata")
}

func metadataProviderRank(key string) int {
	lower := strings.ToLower(key)
	if lower == "opencode" || lower == "opencode-zen" || lower == "opencode_zen" {
		return 0
	}
	if strings.Contains(lower, "opencode") {
		return 1
	}
	return 2
}

func numberPointer(object map[string]any, key string) *float64 {
	value, exists := object[key]
	if !exists || value == nil {
		return nil
	}
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	return &number
}

func metadataDeprecated(model map[string]any) bool {
	if boolAt(model, "deprecated") {
		return true
	}
	status := strings.ToLower(firstString(stringAt(model, "status"), stringAt(model, "lifecycle")))
	if status == "deprecated" || status == "retired" || status == "disabled" {
		return true
	}
	return model["deprecated_at"] != nil || model["retirement_date"] != nil
}

func saveMetadataCache(path string, cache modelMetadataCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".models-dev-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		backup := path + ".replace"
		_ = os.Remove(backup)
		if _, statErr := os.Stat(path); statErr == nil {
			if err := os.Rename(path, backup); err != nil {
				return err
			}
		}
		if err := os.Rename(tempPath, path); err != nil {
			_ = os.Rename(backup, path)
			return err
		}
		_ = os.Remove(backup)
		return os.Chmod(path, 0600)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}
