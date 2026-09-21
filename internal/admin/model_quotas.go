package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

type quotaWindow struct {
	Status           string    `json:"status"`
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetsAt         time.Time `json:"resets_at"`
}

type quotaAccount struct {
	ID        string                 `json:"id"`
	Display   string                 `json:"display"`
	Status    string                 `json:"status"`
	Error     string                 `json:"error,omitempty"`
	FetchedAt *time.Time             `json:"fetched_at,omitempty"`
	CheckedAt time.Time              `json:"checked_at"`
	Windows   map[string]quotaWindow `json:"windows,omitempty"`
}

type quotaCacheEntry struct {
	Account quotaAccount
	Expires time.Time
}

// The first-party endpoint returns subscription-wide percentages, not balances
// for individual models. Never infer model dollars or sum keys' shared quotas.
// Reference: anomalyco/opencode packages/console/app/src/routes/zen/go/v1/usage.ts
func decodeQuota(reader io.Reader) (map[string]quotaWindow, error) {
	var body struct {
		Usage map[string]struct {
			Status   string    `json:"status"`
			Percent  *float64  `json:"percent"`
			ResetsAt time.Time `json:"resetsAt"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, 1<<20)).Decode(&body); err != nil {
		return nil, errors.New("OpenCode 返回的额度格式无法识别")
	}
	result := map[string]quotaWindow{}
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		item, ok := body.Usage[name]
		if !ok || item.Percent == nil || *item.Percent < 0 || *item.Percent > 100 || item.ResetsAt.IsZero() || (item.Status != "ok" && item.Status != "rate-limited") {
			return nil, errors.New("OpenCode 返回的额度窗口不完整")
		}
		result[name] = quotaWindow{Status: item.Status, UsedPercent: *item.Percent, RemainingPercent: 100 - *item.Percent, ResetsAt: item.ResetsAt}
	}
	return result, nil
}

func fetchQuota(ctx context.Context, endpoint, key string) (map[string]quotaWindow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("额度查询地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "opencode2api-admin/1.0")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-opencode-session", "opencode2api-quota-"+config.Fingerprint(key))
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("暂时无法连接 OpenCode，请稍后刷新")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case 401:
			return nil, errors.New("Go API Key 无效或已被撤销")
		case 403:
			return nil, errors.New("此 Key 未获准读取 Go 套餐额度")
		case 429:
			return nil, errors.New("额度查询过于频繁，请稍后刷新")
		default:
			return nil, fmt.Errorf("OpenCode 额度接口返回 HTTP %d", resp.StatusCode)
		}
	}
	return decodeQuota(resp.Body)
}

func refreshQuota(ctx context.Context, endpoint, key string, previous quotaCacheEntry) quotaCacheEntry {
	now := time.Now().UTC()
	account := previous.Account
	account.ID, account.Display = config.Fingerprint(key), config.MaskValue(key)
	account.CheckedAt = now
	windows, err := fetchQuota(ctx, endpoint, key)
	if err != nil {
		account.Status, account.Error = "unavailable", err.Error()
		if account.FetchedAt != nil {
			account.Status = "stale"
		}
		return quotaCacheEntry{Account: account, Expires: now.Add(15 * time.Second)}
	}
	account.Status, account.Error, account.Windows = "ok", "", windows
	account.FetchedAt = &now
	return quotaCacheEntry{Account: account, Expires: now.Add(60 * time.Second)}
}

func (a *Server) handleModelQuotas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cfg := a.manager.Config()
	endpoint := strings.TrimRight(cfg.Upstream.Go, "/") + "/v1/usage"
	accounts := make([]quotaAccount, len(cfg.GoKeys))
	// Coalesce refreshes from multiple browser tabs; at most four upstream
	// queries run at a time. The cache is pruned when upstream credentials change.
	a.quotaMu.Lock()
	if a.quotaCache == nil {
		a.quotaCache = map[string]quotaCacheEntry{}
	}
	entries := make([]quotaCacheEntry, len(cfg.GoKeys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, key := range cfg.GoKeys {
		cacheID := endpoint + "|" + config.Fingerprint(key)
		previous := a.quotaCache[cacheID]
		if time.Now().Before(previous.Expires) {
			entries[i] = previous
			continue
		}
		wg.Add(1)
		go func(i int, key string, previous quotaCacheEntry) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				return
			}
			entries[i] = refreshQuota(r.Context(), endpoint, key, previous)
		}(i, key, previous)
	}
	wg.Wait()
	newCache := map[string]quotaCacheEntry{}
	for i, key := range cfg.GoKeys {
		newCache[endpoint+"|"+config.Fingerprint(key)] = entries[i]
		accounts[i] = entries[i].Account
	}
	a.quotaCache = newCache
	a.quotaMu.Unlock()
	all, _ := a.manager.DebugModels()
	type modelView struct {
		ID       string `json:"id"`
		Protocol string `json:"protocol"`
	}
	models := []modelView{}
	for _, model := range all {
		if model.AvailableGo && model.RouteError == "" {
			protocol := model.NativeProtocols[config.TierGo]
			models = append(models, modelView{ID: model.Model, Protocol: string(protocol)})
		}
	}
	httpx.WriteJSON(w, 200, map[string]any{
		"models": models, "accounts": accounts, "scope": "shared_subscription", "source": endpoint,
		"cache_seconds": 60, "catalog": a.manager.Resources().Models,
		"note": "OpenCode 返回套餐共享额度，未提供逐模型独立余额；多个 Go Key 可能属于同一套餐，不能相加。百分比由上游取整。",
	})
}
