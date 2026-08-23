package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxRequestBody = 32 << 20

const anonymousZenKey = "public"

const (
	proxyHealthCheckURL      = "https://cloudflare.com/cdn-cgi/trace"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

type debugKeyOverride struct {
	Tier  Tier
	KeyID string
}

type debugKeyOverrideContextKey struct{}

func withDebugKeyOverride(ctx context.Context, override debugKeyOverride) context.Context {
	return context.WithValue(ctx, debugKeyOverrideContextKey{}, override)
}

func debugKeyOverrideFromContext(ctx context.Context) (debugKeyOverride, bool) {
	override, ok := ctx.Value(debugKeyOverrideContextKey{}).(debugKeyOverride)
	return override, ok && override.KeyID != ""
}

type Gateway struct {
	cfg        Config
	logger     *slog.Logger
	transports *transportPool
	zenNodes   *nodePool
	goNodes    *nodePool
	anonymous  *anonymousPool
	catalog    *modelCatalog
	monitor    *Monitor
}

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

func NewGateway(cfg Config, logger *slog.Logger, monitor *Monitor) (*Gateway, error) {
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, time.Duration(cfg.Retry.TimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := newNodePool(cfg.ZenKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("zen node pool: %w", err)
	}
	goNodes, err := newNodePool(cfg.GoKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("go node pool: %w", err)
	}
	return &Gateway{
		cfg:        cfg,
		logger:     logger,
		transports: transports,
		zenNodes:   zenNodes,
		goNodes:    goNodes,
		anonymous:  newAnonymousPool(cfg.Anonymous, transports, cooldown),
		catalog:    newModelCatalog(cfg.Prefer, cfg.Models.Protocols),
		monitor:    monitor,
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.handleInference(ProtocolChat)))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.handleInference(ProtocolResponses)))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.handleInference(ProtocolAnthropic)))
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return recoveryMiddleware(g.logger, mux)
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	models := g.catalog.Snapshot()
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
		} else if time.Since(models.UpdatedAt) > staleAfter {
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
	httpStatus := http.StatusOK
	if len(issues) > 0 {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
		if modelStatus == "pending" {
			status = "starting"
		}
	}
	writeJSON(w, httpStatus, healthResponse{
		Status:  status,
		Ready:   len(issues) == 0,
		Version: version,
		Models: healthModels{
			Status:            modelStatus,
			Total:             models.Total,
			Exposed:           models.Exposed,
			Zen:               models.Zen,
			Go:                models.Go,
			LastRefresh:       lastRefresh,
			StaleAfterSeconds: int(staleAfter / time.Second),
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

func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{strings.TrimSpace(r.Header.Get("x-api-key"))}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(auth[7:]))
		}
		valid := false
		for _, key := range g.cfg.ServerKeys {
			for _, candidate := range candidates {
				if len(candidate) == len(key) && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
					valid = true
				}
			}
		}
		if !valid {
			protocol := ProtocolChat
			if r.URL.Path == "/v1/messages" {
				protocol = ProtocolAnthropic
			}
			writeAPIError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		next(w, r)
	}
}

func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	models := g.catalog.List()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !supportedModel(model) {
			continue
		}
		if g.cfg.Anonymous && len(g.cfg.ZenKeys) == 0 && len(g.cfg.GoKeys) == 0 && !g.catalog.anonymousDecision(model).Allowed {
			continue
		}
		data = append(data, map[string]any{"id": model, "object": "model", "created": now, "owned_by": "opencode"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) handleInference(external Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := stringAt(payload, "model")
		meta := metaFromRequest(r)
		if meta != nil {
			meta.Model = model
		}
		if model == "" {
			writeAPIError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if !supportedModel(model) {
			writeAPIError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			return
		}
		override, selected := debugKeyOverrideFromContext(r.Context())
		var route modelRoute
		if selected {
			route, err = g.catalog.RouteForTier(model, external, override.Tier, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0)
		} else {
			route, err = g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
		}
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		if meta != nil {
			meta.Tier = string(route.Tier)
		}
		upstreamURL := g.cfg.Upstream.Zen
		if route.Tier == TierGo {
			upstreamURL = g.cfg.Upstream.Go
		}
		upstreamPayload, err := prepareUpstreamRequest(external, route.Protocol, payload, upstreamURL)
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}
		encoded, err := json.Marshal(upstreamPayload)
		if err != nil {
			writeAPIError(w, external, http.StatusBadRequest, "request contains unsupported JSON values", "invalid_request_error", "")
			return
		}
		ids := deriveRequestIDs(r, payload)
		if meta != nil {
			meta.Request = ids.Request
		}
		stream := boolAt(payload, "stream")
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		resp, err := g.doUpstream(requestCtx, route, encoded, ids)
		if err != nil {
			finalTier := route.Tier
			if meta != nil && meta.Tier != "" {
				finalTier = Tier(meta.Tier)
			}
			g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "tier", finalTier, "error", err)
			writeAPIError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		if stream {
			if meta != nil {
				meta.Stream = true
			}
			if g.monitor != nil {
				g.monitor.activeStreams.Add(1)
				defer g.monitor.activeStreams.Add(-1)
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			var usage bridgeUsage
			var usageReported bool
			if external == route.Protocol {
				observer := newStreamUsageObserver(route.Protocol)
				_, err = io.Copy(w, io.TeeReader(resp.Body, observer))
				usage = observer.Finish()
				usageReported = observer.Reported()
			} else {
				usage, usageReported, err = transcodeStreamWithUsage(w, resp.Body, route.Protocol, external, model)
			}
			if meta != nil {
				meta.Usage, meta.UsageReported = usage, usageReported
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Debug("downstream stream ended with an error", "component", "stream", "event", "stream_failed", "request_id", ids.Request, "model", model, "tier", route.Tier, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			writeAPIError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		if usage, reported := extractResponseUsage(route.Protocol, responseBody); meta != nil {
			meta.Usage, meta.UsageReported = usage, reported
		}
		if external != route.Protocol {
			responseBody, err = convertResponse(route.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response protocol conversion failed", "component", "conversion", "event", "response_conversion_failed", "request_id", ids.Request, "model", model, "source_protocol", route.Protocol, "target_protocol", external, "error", err)
				writeAPIError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

func (g *Gateway) doUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs) (*http.Response, error) {
	if override, selected := debugKeyOverrideFromContext(ctx); selected {
		return g.doSelectedKeyUpstream(ctx, route, body, ids, override)
	}
	var lastResponse *http.Response
	var lastErr error
	attempts := 0
	if route.Anonymous {
		resp, err, used := g.doAnonymousUpstream(ctx, route, body, ids)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		lastResponse, lastErr = resp, err
		if len(route.KeyTiers) > 0 {
			g.logger.Debug("anonymous request did not succeed; entering preferred key tiers", "component", "upstream", "event", "anonymous_fallback", "request_id", ids.Request, "attempts", attempts, "key_tiers", route.KeyTiers)
		}
	}

	keyTiers := route.KeyTiers
	if !route.Anonymous && len(keyTiers) == 0 && (route.Tier == TierZen || route.Tier == TierGo) {
		keyTiers = []Tier{route.Tier}
	}
	for _, tier := range keyTiers {
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		keyRoute := route
		keyRoute.Tier = tier
		keyRoute.Anonymous = false
		resp, err, used := g.doKeyUpstream(ctx, keyRoute, body, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		lastResponse, lastErr = resp, err
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable upstream route")
	}
	return nil, lastErr
}

// doAnonymousUpstream tries every currently available proxy at most once. Any
// failure, including an HTTP error response, advances to the next proxy. Only a
// successful response ends the anonymous phase; exhausting the proxy cursor
// returns control to the preferred authenticated tiers.
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	cursor := g.anonymous.CursorFor(ids.Session)
	limit := g.anonymous.Len()
	attempts := 0
	for attempts < limit {
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		if meta, _ := ctx.Value(requestMetaKey{}).(*requestMeta); meta != nil {
			meta.Attempts = attempts
			meta.Tier = string(TierZen)
		}
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		req, err := newUpstreamRequest(ctx, g.cfg.Upstream.Zen, route.Protocol, body, ids, anonymousZenKey)
		if err != nil {
			return nil, err, attempts
		}
		started := time.Now()
		resp, err := node.proxy.client.Do(req)
		duration := time.Since(started)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		g.syncProxyResult(ctx, node.proxy, status, err)
		g.recordUpstreamAttempt(route, ids, attempts, "anonymous", "anonymous", true, node.proxy, resp, err, duration)
		if err == nil && resp.StatusCode/100 == 2 {
			g.anonymous.MarkSuccess(node)
			g.logger.Debug("anonymous upstream accepted request", "component", "upstream", "event", "anonymous_attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "tier", TierZen, "proxy", redactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
			return resp, nil, attempts
		}
		g.anonymous.MarkFailure(node, resp, err)
		lastResponse = resp
		lastErr = err
		if err != nil {
			g.logger.Debug("anonymous transport attempt failed", "component", "upstream", "event", "anonymous_attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "tier", TierZen, "proxy", redactURL(node.proxy.name), "duration_ms", duration.Milliseconds(), "error", err)
		} else {
			g.logger.Debug("anonymous upstream returned an error response; trying the next proxy", "component", "upstream", "event", "anonymous_attempt_response_failed", "request_id", ids.Request, "attempt", attempts, "tier", TierZen, "proxy", redactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
		}
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy anonymous proxies available")
	}
	return nil, lastErr, attempts
}

func (g *Gateway) doKeyUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if route.Tier == TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	cursor := nodes.CursorFor(ids.Session)
	if nodes.Len() == 0 {
		return nil, fmt.Errorf("no %s nodes configured", route.Tier), 0
	}
	attempts := 0
	for attempts < g.cfg.Retry.MaxAttempts {
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		// Attempts are exposed only as aggregate request metadata and never
		// include the selected credential.
		if meta, _ := ctx.Value(requestMetaKey{}).(*requestMeta); meta != nil {
			meta.Attempts = attemptOffset + attempts
			meta.Tier = string(route.Tier)
		}
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		req, err := newUpstreamRequest(ctx, baseURL, route.Protocol, body, ids, node.key)
		if err != nil {
			return nil, err, attempts
		}
		proxy := nodes.Proxy(node)
		if proxy == nil {
			lastErr = errors.New("upstream key has no proxy binding")
			break
		}
		attemptStarted := time.Now()
		resp, err := proxy.client.Do(req)
		attemptDuration := time.Since(attemptStarted)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		proxyFailed := g.syncProxyResult(ctx, proxy, status, err)
		g.recordUpstreamAttempt(route, ids, attemptOffset+attempts, secretFingerprint(node.key), "key", false, proxy, resp, err, attemptDuration)
		if err == nil && resp.StatusCode/100 == 2 {
			nodes.MarkSuccess(node)
			g.logger.Debug("upstream accepted request", "component", "upstream", "event", "attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", secretFingerprint(node.key), "proxy", redactURL(proxy.name), "status", resp.StatusCode, "duration_ms", attemptDuration.Milliseconds())
			return resp, nil, attempts
		}
		// Request-shape errors are deterministic and must leave this tier without
		// rotating through unrelated keys. The outer route may still try the next
		// tier in prefer order. Authentication, throttling, server, and transport
		// failures remain retryable inside this tier.
		if isNonRetryableClientResponse(resp, err) {
			nodes.MarkSuccess(node)
			g.logger.Debug("upstream rejected a non-retryable request", "component", "upstream", "event", "attempt_rejected", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", secretFingerprint(node.key), "status", resp.StatusCode, "proxy", redactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds())
			return resp, nil, attempts
		}
		if proxyFailed {
			if nodes.Proxy(node) == proxy {
				nodes.MarkFailure(node, resp, err)
			}
		} else {
			nodes.MarkFailure(node, resp, err)
		}
		lastResponse = resp
		lastErr = err
		if err != nil {
			g.logger.Debug("upstream transport attempt failed", "component", "upstream", "event", "attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", secretFingerprint(node.key), "proxy", redactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds(), "error", err)
		} else {
			g.logger.Debug("upstream returned a retryable response", "component", "upstream", "event", "attempt_retryable_response", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", secretFingerprint(node.key), "status", resp.StatusCode, "proxy", redactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds())
		}
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	return nil, lastErr, attempts
}

func (g *Gateway) doSelectedKeyUpstream(ctx context.Context, route modelRoute, body []byte, ids requestIDs, override debugKeyOverride) (*http.Response, error) {
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if override.Tier == TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	node := nodes.NodeByID(override.KeyID)
	if node == nil {
		return nil, fmt.Errorf("selected %s key is no longer configured", override.Tier)
	}
	if meta, _ := ctx.Value(requestMetaKey{}).(*requestMeta); meta != nil {
		meta.Attempts = 1
		meta.Tier = string(route.Tier)
	}
	req, err := newUpstreamRequest(ctx, baseURL, route.Protocol, body, ids, node.key)
	if err != nil {
		return nil, err
	}
	proxy := nodes.Proxy(node)
	if proxy == nil {
		return nil, errors.New("selected upstream key has no proxy binding")
	}
	started := time.Now()
	resp, err := proxy.client.Do(req)
	duration := time.Since(started)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	proxyFailed := g.syncProxyResult(ctx, proxy, status, err)
	g.recordUpstreamAttempt(route, ids, 1, secretFingerprint(node.key), "key", false, proxy, resp, err, duration)
	if err == nil && resp != nil && resp.StatusCode/100 == 2 {
		nodes.MarkSuccess(node)
		return resp, nil
	}
	if proxyFailed {
		if nodes.Proxy(node) == proxy {
			nodes.MarkFailure(node, resp, err)
		}
	} else {
		nodes.MarkFailure(node, resp, err)
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func extractResponseUsage(protocol Protocol, body []byte) (bridgeUsage, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return bridgeUsage{}, false
	}
	usage := mapAt(payload, "usage")
	if len(usage) == 0 {
		return bridgeUsage{}, false
	}
	if protocol == ProtocolAnthropic {
		return decodeAnthropicUsage(usage), true
	}
	return decodeOpenAIUsage(usage), true
}

func (g *Gateway) recordUpstreamAttempt(route modelRoute, ids requestIDs, attempt int, keyID, channel string, anonymous bool, proxy *proxyTransport, resp *http.Response, err error, duration time.Duration) {
	if g.monitor == nil {
		return
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	success := err == nil && status >= 200 && status < 300
	outcome := "retryable_failure"
	if success {
		outcome = "success"
	} else if err != nil {
		outcome = "transport_error"
	} else if isNonRetryableClientResponse(resp, nil) {
		outcome = "rejected"
	}
	proxyName := "unavailable"
	if proxy != nil {
		proxyName = redactURL(proxy.name)
	}
	g.monitor.RecordAttempt(UpstreamAttempt{
		Time: time.Now().UTC(), RequestID: ids.Request, Model: route.ID, Tier: string(route.Tier), Attempt: attempt,
		KeyID: keyID, Channel: channel, Anonymous: anonymous, Proxy: proxyName, Status: status,
		DurationMS: max(duration.Milliseconds(), 0), Success: success, Outcome: outcome,
	})
}

func newUpstreamRequest(ctx context.Context, baseURL string, protocol Protocol, body []byte, ids requestIDs, key string) (*http.Request, error) {
	endpoint := strings.TrimRight(baseURL, "/") + protocolPath(protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", opencodeUserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", ids.Session)
	req.Header.Set("x-opencode-request", ids.Request)
	req.Header.Set("x-opencode-project", ids.Project)
	if protocol == ProtocolAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func isNonRetryableClientResponse(resp *http.Response, err error) bool {
	return err == nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests
}

// syncProxyResult updates proxy health from real traffic. Only timeouts and
// connection refusals mark a proxy unavailable. Other errors and 4xx/5xx
// responses trigger a neutral URL check without being treated as proxy failure.
func (g *Gateway) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil {
		return false
	}
	if isProxyFailure(err) {
		g.rebindFailedProxy(proxy)
		g.verifyProxyAfterError(ctx, proxy, status)
		return true
	}
	if status >= 200 && status < 400 {
		wasHealthy := proxy.healthy.Swap(true)
		if !wasHealthy {
			g.restoreProxy(proxy)
		}
		return false
	}
	if err != nil {
		g.verifyProxyAfterError(ctx, proxy, status)
		return false
	}
	if status >= 400 && status < 600 {
		g.verifyProxyAfterError(ctx, proxy, status)
	}
	return false
}

func (g *Gateway) verifyProxyAfterError(ctx context.Context, proxy *proxyTransport, status int) {
	if !proxy.checking.CompareAndSwap(false, true) {
		return
	}
	// The client request may finish or be cancelled while the verification is
	// running. Keep its values but give the proxy check an independent timeout.
	checkCtx := context.WithoutCancel(ctx)
	go func() {
		result := g.transports.checkClaimedProxy(checkCtx, proxy, proxyHealthCheckURL, proxyHealthCheckTimeout)
		g.applyProxyHealthResult(result, "upstream HTTP response", status)
	}()
}

func (g *Gateway) rebindFailedProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	wasHealthy := proxy.healthy.Swap(false)
	return g.rebindUnavailableProxy(proxy, wasHealthy)
}

func (g *Gateway) rebindUnavailableProxy(proxy *proxyTransport, wasHealthy bool) (zenMoved, goMoved int) {
	zenMoved = g.zenNodes.RebindProxy(proxy.index)
	goMoved = g.goNodes.RebindProxy(proxy.index)
	if wasHealthy || zenMoved+goMoved > 0 {
		g.logger.Warn("proxy became unavailable", "component", "proxy", "event", "proxy_unavailable", "proxy", redactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (g *Gateway) restoreProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	zenMoved = g.zenNodes.RestoreProxy(proxy.index)
	goMoved = g.goNodes.RestoreProxy(proxy.index)
	if zenMoved+goMoved > 0 {
		g.logger.Info("proxy connectivity restored", "component", "proxy", "event", "proxy_restored", "proxy", redactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (g *Gateway) StartProxyHealthChecks(ctx context.Context) {
	check := func() {
		results := g.transports.CheckHealth(ctx, proxyHealthCheckURL, proxyHealthCheckTimeout)
		for _, result := range results {
			g.applyProxyHealthResult(result, "scheduled health check", 0)
		}
	}
	go func() {
		ticker := time.NewTicker(proxyHealthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

func (g *Gateway) applyProxyHealthResult(result proxyHealthResult, source string, upstreamStatus int) {
	if result.err == nil {
		if !result.wasHealthy {
			g.restoreProxy(result.proxy)
		}
		g.logger.Debug("proxy health check passed", "component", "proxy", "event", "health_check_passed", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name))
		return
	}
	if !result.failed {
		g.logger.Debug("proxy health check was inconclusive", "component", "proxy", "event", "health_check_inconclusive", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "error", result.err)
		return
	}
	if g.transports.hasHealthy() {
		zenMoved, goMoved := g.rebindUnavailableProxy(result.proxy, result.wasHealthy)
		if result.wasHealthy || zenMoved+goMoved > 0 {
			g.logger.Warn("proxy health check failed", "component", "proxy", "event", "health_check_failed", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved, "error", result.err)
			return
		}
	}
	g.logger.Debug("proxy health check is still failing", "component", "proxy", "event", "health_check_still_failing", "source", source, "upstream_status", upstreamStatus, "proxy", redactURL(result.proxy.name), "error", result.err)
}

func protocolPath(protocol Protocol) string {
	switch protocol {
	case ProtocolResponses:
		return "/v1/responses"
	case ProtocolAnthropic:
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

func (g *Gateway) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		var zen, goModels []string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); zen = g.refreshZen(ctx) }()
		go func() { defer wg.Done(); goModels = g.refreshTier(ctx, g.cfg.Upstream.Go, g.goNodes) }()
		wg.Wait()
		if zen != nil || goModels != nil {
			g.catalog.Replace(zen, goModels)
			g.logger.Info("model catalog refreshed", "component", "models", "event", "catalog_refreshed", "models", len(g.catalog.List()))
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(time.Duration(g.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (g *Gateway) refreshZen(ctx context.Context) []string {
	if models := g.refreshTier(ctx, g.cfg.Upstream.Zen, g.zenNodes); models != nil {
		return models
	}
	if !g.cfg.Anonymous {
		return nil
	}
	return g.refreshAnonymousTier(ctx, g.cfg.Upstream.Zen)
}

func (g *Gateway) refreshAnonymousTier(ctx context.Context, base string) []string {
	cursor := g.anonymous.CursorFor("")
	limit := g.anonymous.Len()
	for attempt := 1; attempt <= limit; attempt++ {
		node := cursor.Next()
		if node == nil {
			break
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, status, err := fetchModels(refreshCtx, node.proxy.client, base, anonymousZenKey)
		g.syncProxyResult(refreshCtx, node.proxy, status, err)
		cancel()
		if err == nil {
			g.anonymous.MarkSuccess(node)
			return models
		}
		g.anonymous.MarkFailure(node, nil, err)
		g.logger.Debug("anonymous model catalog refresh attempt failed", "component", "models", "event", "anonymous_refresh_attempt_failed", "upstream", redactURL(base), "attempt", attempt, "proxy", redactURL(node.proxy.name), "error", err)
	}
	g.logger.Warn("anonymous model catalog refresh failed", "component", "models", "event", "anonymous_refresh_failed", "upstream", redactURL(base))
	return nil
}

func (g *Gateway) refreshTier(ctx context.Context, base string, nodes *nodePool) []string {
	cursor := nodes.Cursor()
	for attempt := 0; attempt < g.cfg.Retry.MaxAttempts; attempt++ {
		node := cursor.Next()
		if node == nil {
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		proxy := nodes.Proxy(node)
		if proxy == nil {
			cancel()
			return nil
		}
		models, status, err := fetchModels(refreshCtx, proxy.client, base, node.key)
		g.syncProxyResult(refreshCtx, proxy, status, err)
		cancel()
		if err == nil {
			nodes.MarkSuccess(node)
			return models
		}
		nodes.MarkFailure(node, nil, err)
		g.logger.Debug("model catalog refresh attempt failed", "component", "models", "event", "refresh_attempt_failed", "upstream", redactURL(base), "attempt", attempt+1, "error", err)
	}
	g.logger.Warn("model catalog refresh failed", "component", "models", "event", "refresh_failed", "upstream", redactURL(base))
	return nil
}

func copyErrorResponse(w http.ResponseWriter, protocol Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = firstString(stringAt(value, "error", "message"), stringAt(value, "message"), message)
	}
	writeAPIError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}

func writeAPIError(w http.ResponseWriter, protocol Protocol, status int, message, kind, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("x-request-id", requestID)
	}
	if protocol == ProtocolAnthropic {
		writeJSONStatus(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
		return
	}
	writeJSONStatus(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": nil}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	writeJSONStatus(w, status, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func recoveryMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request handler panicked", "component", "http", "event", "request_panic", "error", value)
				writeAPIError(w, ProtocolChat, http.StatusInternalServerError, "internal server error", "server_error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
