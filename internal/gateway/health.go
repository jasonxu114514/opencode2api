package gateway

import (
	"net/http"
	"time"

	"opencode2api/internal/buildinfo"
	"opencode2api/internal/httpx"
	modelcatalog "opencode2api/internal/models"
)

type healthResponse struct {
	Status  string        `json:"status"`
	Ready   bool          `json:"ready"`
	Version string        `json:"version"`
	Models  healthModels  `json:"models"`
	Keys    healthKeys    `json:"keys"`
	Proxies healthProxies `json:"proxies"`
	Issues  []string      `json:"issues,omitempty"`
}

type healthModels struct {
	Status            string     `json:"status"`
	Total             int        `json:"total"`
	Exposed           int        `json:"exposed"`
	Zen               int        `json:"zen"`
	Go                int        `json:"go"`
	LastRefresh       *time.Time `json:"last_refresh,omitempty"`
	StaleAfterSeconds int        `json:"stale_after_seconds"`
	CacheSource       string     `json:"cache_source,omitempty"`
	Stale             bool       `json:"stale"`
}

type healthKeys struct {
	Zen       int  `json:"zen"`
	Go        int  `json:"go"`
	Total     int  `json:"total"`
	Anonymous bool `json:"anonymous"`
}

type healthProxies struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	_, models := g.availableModels()
	proxyTotal, proxyHealthy := g.transports.healthCounts()
	zenKeys, goKeys := g.zenNodes.Len(), g.goNodes.Len()
	staleAfter := max(2*time.Duration(g.cfg.Models.RefreshSeconds)*time.Second, time.Minute)

	modelStatus := "ready"
	var lastRefresh *time.Time
	issues := make([]string, 0, 3)
	if models.UpdatedAt.IsZero() {
		modelStatus = "pending"
		issues = append(issues, "model_catalog_pending")
	} else {
		updatedAt := models.UpdatedAt.UTC()
		lastRefresh = &updatedAt
		if models.Exposed == 0 {
			modelStatus = "empty"
			issues = append(issues, "model_catalog_empty")
		} else if models.Stale || time.Since(models.UpdatedAt) > staleAfter {
			modelStatus = "stale"
			issues = append(issues, "model_catalog_stale")
		}
	}
	if zenKeys+goKeys == 0 && !g.cfg.Anonymous {
		issues = append(issues, "no_upstream_keys")
	}
	if proxyHealthy == 0 {
		issues = append(issues, "no_healthy_proxies")
	}

	status := "ok"
	if len(issues) > 0 {
		status = "degraded"
	}
	blocking := modelStatus == "pending" || modelStatus == "empty" || proxyHealthy == 0
	httpStatus := http.StatusOK
	ready := !blocking
	if blocking {
		httpStatus = http.StatusServiceUnavailable
		if modelStatus == "pending" {
			status = "starting"
		}
	}
	httpx.WriteJSON(w, httpStatus, healthResponse{
		Status:  status,
		Ready:   ready,
		Version: buildinfo.Version,
		Models: healthModels{
			Status:            modelStatus,
			Total:             models.Total,
			Exposed:           models.Exposed,
			Zen:               models.Zen,
			Go:                models.Go,
			LastRefresh:       lastRefresh,
			StaleAfterSeconds: int(staleAfter / time.Second),
			CacheSource:       models.CacheSource,
			Stale:             models.Stale,
		},
		Keys: healthKeys{Zen: zenKeys, Go: goKeys, Total: zenKeys + goKeys, Anonymous: g.cfg.Anonymous},
		Proxies: healthProxies{
			Total:     proxyTotal,
			Healthy:   proxyHealthy,
			Unhealthy: proxyTotal - proxyHealthy,
		},
		Issues: issues,
	})
}

func (g *Gateway) availableModels() ([]modelcatalog.Route, modelcatalog.CatalogSnapshot) {
	return g.catalog.AvailableModels(g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	routes, _ := g.availableModels()
	data := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		model := route.ID
		// Tier-scoped metadata: the advertised context window must match
		// the tier that will serve the request (anonymous ⇒ Zen).
		md := g.catalog.MetadataForTier(model, route.Tier)
		entry := map[string]any{
			"id": model, "object": "model", "created": now, "owned_by": "opencode",
			"metadata": md,
		}
		// Top-level OpenAI-standard fields: discovery clients (jcode, Pi, …)
		// read context/reasoning at the top level of each model entry.
		if md.ContextWindow > 0 {
			entry["context_window"] = md.ContextWindow
			entry["context_length"] = md.ContextWindow
		}
		if md.MaxInput > 0 {
			entry["max_input"] = md.MaxInput
		}
		if md.MaxOutput > 0 {
			entry["max_output"] = md.MaxOutput
		}
		if md.Reasoning {
			entry["reasoning"] = true
			entry["supports_reasoning"] = true
		}
		if md.ToolCall {
			entry["tool_call"] = true
		}
		if md.StructuredOutput {
			entry["structured_output"] = true
		}
		data = append(data, entry)
	}
	for _, group := range g.rotation.Snapshot().Groups {
		data = append(data, map[string]any{"id": group.Alias, "object": "model", "created": now, "owned_by": "opencode-go-rotation"})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
