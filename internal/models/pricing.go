package models

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

	"opencode2api/internal/httpx"
	"opencode2api/internal/jsonutil"
)

// metadataDefaultURL is the OpenCode metadata endpoint, metadataRefresh the
// interval between refreshes, metadataRetry the delay after a failed refresh,
// and metadataTimeout the limit of one fetch.
const (
	metadataDefaultURL = CapabilitiesURL
	metadataRefresh    = 24 * time.Hour
	metadataRetry      = time.Minute
	metadataTimeout    = 30 * time.Second
)

// Price is the pricing and limit metadata of one model. A nil cost means the
// metadata did not state it.
type Price struct {
	ID            string   `json:"id"`
	Input         *float64 `json:"input_cost,omitempty"`
	Output        *float64 `json:"output_cost,omitempty"`
	CacheRead     *float64 `json:"cache_read_cost,omitempty"`
	CacheWrite    *float64 `json:"cache_write_cost,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxInput      int      `json:"max_input,omitempty"`
	MaxOutput     int      `json:"max_output,omitempty"`
	Deprecated    bool     `json:"deprecated"`
}

// AnonymousDecision reports whether a model counts as free, which admits it to
// the anonymous lane and keeps it visible under models.free_only. Source names
// the rule that decided, such as "name_free", "metadata_free" or
// "metadata_paid". Known reports whether the metadata lists the model; it is
// also false for "metadata_cost_unknown".
type AnonymousDecision struct {
	Allowed    bool     `json:"allowed"`
	Source     string   `json:"source"`
	Known      bool     `json:"known"`
	Deprecated bool     `json:"deprecated"`
	InputCost  *float64 `json:"input_cost,omitempty"`
	OutputCost *float64 `json:"output_cost,omitempty"`
}

// MetadataSnapshot reports the freshness of the pricing metadata. Ready means
// models were loaded from the cache or the endpoint. Stale means the last
// successful refresh is older than the refresh interval. NextRefresh is the
// next scheduled refresh, or nil when the refresh loop has not started.
type MetadataSnapshot struct {
	Ready       bool       `json:"ready"`
	Models      int        `json:"models"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	Stale       bool       `json:"stale"`
	LastError   string     `json:"last_error,omitempty"`
	CachePath   string     `json:"-"`
	NextRefresh *time.Time `json:"next_refresh,omitempty"`
}

// modelMetadataCache is the on-disk copy of the last successful refresh.
type modelMetadataCache struct {
	UpdatedAt time.Time        `json:"updated_at"`
	Models    map[string]Price `json:"models"`
}

// PricingRegistry holds the per-model pricing and limit metadata from the
// OpenCode metadata endpoint, keyed by model ID. It decides which models are
// free and keeps the last good response in a cache file so a restart has data
// before the first refresh.
type PricingRegistry struct {
	mu             sync.RWMutex
	models         map[string]Price
	updatedAt      time.Time
	nextRefresh    time.Time
	lastError      string
	cachePath      string
	endpoint       string
	client         *http.Client
	clientProvider func() []*http.Client
	logger         *slog.Logger
}

// PricingStore creates a PricingRegistry whose cache file lives beside
// configPath (none when configPath is empty) and loads that cache. A missing
// cache is not an error; any other load failure is kept as the snapshot's
// LastError.
func PricingStore(configPath string, logger *slog.Logger) *PricingRegistry {
	cachePath := ""
	if configPath != "" {
		// The suffix predates the switch to the OpenCode metadata endpoint and
		// is kept so existing caches keep loading after an upgrade.
		cachePath = configPath + ".models.dev.json"
	}
	store := &PricingRegistry{
		models: make(map[string]Price), cachePath: cachePath, endpoint: metadataDefaultURL,
		client: &http.Client{Timeout: metadataTimeout}, logger: logger,
	}
	if err := store.loadCache(); err != nil && !errors.Is(err, os.ErrNotExist) {
		store.lastError = "load metadata cache: " + err.Error()
	}
	return store
}

// SetClientProvider supplies candidate HTTP clients for refresh attempts, most
// preferred first. It lets the periodic OpenCode metadata refresh ride the proxy
// transports exposed by whichever gateway runtime is currently active.
func (store *PricingRegistry) SetClientProvider(provider func() []*http.Client) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.clientProvider = provider
}

// refreshClients returns the provider's clients, or the registry's own direct
// client when no provider is set or it offers none.
func (store *PricingRegistry) refreshClients() []*http.Client {
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

// Start refreshes the metadata at once and then periodically until ctx ends.
// Failed refreshes are logged and retried after metadataRetry.
func (store *PricingRegistry) Start(ctx context.Context) {
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			store.setNextRefresh(time.Time{})
			delay := metadataRetry
			if store.refreshAndLog(ctx) {
				delay = metadataRefresh
			}
			store.setNextRefresh(time.Now().Add(delay))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				store.setNextRefresh(time.Time{})
				return
			case <-timer.C:
				store.setNextRefresh(time.Time{})
			}
		}
	}()
}

func (store *PricingRegistry) setNextRefresh(next time.Time) {
	store.mu.Lock()
	store.nextRefresh = next
	store.mu.Unlock()
}

func (store *PricingRegistry) refreshAndLog(ctx context.Context) bool {
	if err := store.Refresh(ctx); err != nil {
		if store.logger != nil {
			store.logger.Warn("OpenCode metadata refresh failed", "component", "models", "event", "metadata_refresh_failed", "error", err)
		}
		return false
	}
	if store.logger != nil {
		store.logger.Info("OpenCode metadata refreshed", "component", "models", "event", "metadata_refreshed", "models", store.Snapshot().Models)
	}
	return true
}

// Refresh fetches the metadata, trying each refresh client in turn until one
// gets a 2xx response. That response is final: a decode or cache write error
// is returned without trying the remaining clients. The cache file is written
// before the in-memory data is replaced, so a failed write keeps the previous
// data. The final failure is also kept as LastError.
func (store *PricingRegistry) Refresh(ctx context.Context) error {
	var lastErr error
	for _, client := range store.refreshClients() {
		data, err := store.fetch(ctx, client)
		if err != nil {
			lastErr = err
			continue
		}
		models, err := decodeMetadata(data)
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
		lastErr = errors.New("no HTTP client available for OpenCode metadata refresh")
	}
	return store.recordError(lastErr)
}

func (store *PricingRegistry) fetch(ctx context.Context, client *http.Client) ([]byte, error) {
	refreshCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(refreshCtx, http.MethodGet, store.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpx.V2UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OpenCode metadata endpoint returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func (store *PricingRegistry) recordError(err error) error {
	store.mu.Lock()
	store.lastError = err.Error()
	store.mu.Unlock()
	return err
}

// Decide reports whether model counts as free. A name containing "free" in any
// case always qualifies. Otherwise the metadata must list the model, not
// deprecated, at zero input and output cost; until the metadata is ready only
// the name rule applies.
func (store *PricingRegistry) Decide(model string) AnonymousDecision {
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

// Price returns the metadata of model and whether the metadata lists it.
func (store *PricingRegistry) Price(model string) (Price, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	price, ok := store.models[model]
	return price, ok
}

// Snapshot reports the freshness of the metadata; see MetadataSnapshot.
func (store *PricingRegistry) Snapshot() MetadataSnapshot {
	store.mu.RLock()
	defer store.mu.RUnlock()
	snapshot := MetadataSnapshot{Ready: !store.updatedAt.IsZero() && len(store.models) > 0, Models: len(store.models), LastError: store.lastError, CachePath: store.cachePath}
	if !store.updatedAt.IsZero() {
		updated := store.updatedAt.UTC()
		snapshot.UpdatedAt = &updated
		snapshot.Stale = time.Since(updated) > metadataRefresh
	}
	if !store.nextRefresh.IsZero() {
		next := store.nextRefresh.UTC()
		snapshot.NextRefresh = &next
	}
	return snapshot
}

// loadCache installs the cache file. It takes no lock, so it must only run
// while PricingStore constructs the registry.
func (store *PricingRegistry) loadCache() error {
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

// decodeMetadata extracts a Price for every model of the OpenCode providers in
// the metadata endpoint's provider map. The opencode, opencode-zen and
// opencode_zen entries rank first; any other provider whose key contains
// "opencode" is read only when its id or name does too, and every other
// provider is ignored. It fails when no OpenCode model is found.
func decodeMetadata(data []byte) (map[string]Price, error) {
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("decode OpenCode metadata: %w", err)
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
	result := make(map[string]Price)
	for _, key := range keys {
		if metadataProviderRank(key) > 1 {
			continue
		}
		var provider map[string]any
		if json.Unmarshal(providers[key], &provider) != nil {
			continue
		}
		if metadataProviderRank(key) == 1 {
			identity := strings.ToLower(jsonutil.FirstString(jsonutil.StringAt(provider, "id"), jsonutil.StringAt(provider, "name")))
			if !strings.Contains(identity, "opencode") {
				continue
			}
		}
		for id, raw := range jsonutil.MapAt(provider, "models") {
			model, _ := raw.(map[string]any)
			modelID := jsonutil.FirstString(jsonutil.StringAt(model, "id"), id)
			// Providers are sorted with OpenCode first, so keep its metadata
			// for duplicate model IDs and use other OpenCode tiers to fill gaps.
			if _, exists := result[modelID]; exists {
				continue
			}
			cost := jsonutil.MapAt(model, "cost")
			limit := jsonutil.MapAt(model, "limit")
			result[modelID] = Price{
				ID: modelID, Input: numberPointer(cost, "input"), Output: numberPointer(cost, "output"),
				CacheRead: numberPointer(cost, "cache_read"), CacheWrite: numberPointer(cost, "cache_write"),
				ContextWindow: jsonutil.IntAt(limit, "context"), MaxInput: jsonutil.IntAt(limit, "input"), MaxOutput: jsonutil.IntAt(limit, "output"),
				Deprecated: metadataDeprecated(model),
			}
		}
	}
	if len(result) > 0 {
		return result, nil
	}
	return nil, errors.New("OpenCode metadata contains no OpenCode models")
}

// metadataProviderRank orders metadata provider keys: 0 for opencode,
// opencode-zen and opencode_zen, 1 for any other key containing "opencode",
// and 2 for the providers decodeMetadata ignores.
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

// metadataDeprecated reports whether a metadata model is flagged deprecated,
// carries a deprecated, retired or disabled status or lifecycle, or names a
// deprecation or retirement date.
func metadataDeprecated(model map[string]any) bool {
	if jsonutil.BoolAt(model, "deprecated") {
		return true
	}
	status := strings.ToLower(jsonutil.FirstString(jsonutil.StringAt(model, "status"), jsonutil.StringAt(model, "lifecycle")))
	if status == "deprecated" || status == "retired" || status == "disabled" {
		return true
	}
	return model["deprecated_at"] != nil || model["retirement_date"] != nil
}

// saveMetadataCache writes cache through a 0600 temporary file and a rename.
// On Windows the existing file is first moved to path+".replace" and put back
// if the final rename fails.
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
