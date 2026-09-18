package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// DebugKeyOverride pins one inference request to a single configured upstream
// key so the Playground can exercise that key in isolation.
type DebugKeyOverride struct {
	Tier  config.Tier
	KeyID string
}

type debugKeyOverrideContextKey struct{}

type diagnosticContextKey struct{}

func WithDiagnosticRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, diagnosticContextKey{}, true)
}

func isDiagnosticRequest(ctx context.Context) bool {
	diagnostic, _ := ctx.Value(diagnosticContextKey{}).(bool)
	return diagnostic
}

func WithDebugKeyOverride(ctx context.Context, override DebugKeyOverride) context.Context {
	return context.WithValue(WithDiagnosticRequest(ctx), debugKeyOverrideContextKey{}, override)
}

func debugKeyOverrideFrom(ctx context.Context) (DebugKeyOverride, bool) {
	override, ok := ctx.Value(debugKeyOverrideContextKey{}).(DebugKeyOverride)
	return override, ok && override.KeyID != ""
}

func (g *Gateway) doUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs) (*http.Response, models.Route, error) {
	resp, effectiveRoute, attempts, err := g.doUpstreamTiers(ctx, route, bodies, ids, 0)
	if _, selected := debugKeyOverrideFrom(ctx); selected {
		return resp, effectiveRoute, err
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, effectiveRoute, err
	}
	origBody := resp.Body
	errBody, readErr := io.ReadAll(io.LimitReader(origBody, 1<<20))
	// The original network body is always closed here: the retry path below
	// reuses the connection, otherwise downstream receives a fresh in-memory
	// reader over the cached bytes.
	httpx.DrainAndClose(origBody)
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, effectiveRoute, nil
	}
	// Restore the body so downstream error handling still sees the original
	// payload when no retry happens below.
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
	if !isStaleReasoningReference(errBody) {
		return resp, effectiveRoute, nil
	}
	stripped, changed := stripStaleReasoningInputs(effectiveRoute, bodies)
	if !changed {
		return resp, effectiveRoute, nil
	}
	// The referenced reasoning items belong to an upstream chain this session
	// can no longer address (e.g. an interrupted stream). Replay once without
	// them instead of failing the client request outright. Attempt numbering
	// continues from the first round so monitoring never shows duplicate attempt
	// numbers for one request.
	//
	// The client's session is deliberately preserved: it carries key/proxy
	// affinity and upstream session correlation. Minting a fresh session made
	// the replay land on a different node with a cold prompt cache and detached
	// the reply from the chain the client still holds; the stale references are
	// already removed from the payload itself.
	g.logger.Info("retrying upstream without stale reasoning references", "component", "upstream", "event", "reasoning_reference_retry", "request_id", ids.Request, "model", route.ID, "tier", effectiveRoute.Tier, "attempt_offset", attempts)
	retryResp, retryRoute, _, retryErr := g.doUpstreamTiers(ctx, effectiveRoute, stripped, ids, attempts)
	if retryErr != nil || retryResp == nil || retryResp.StatusCode/100 != 2 {
		retryStatus := 0
		if retryResp != nil {
			retryStatus = retryResp.StatusCode
			httpx.DrainAndClose(retryResp.Body)
		}
		g.logger.Warn("reasoning reference retry failed; returning original error", "component", "upstream", "event", "reasoning_reference_retry_failed", "request_id", ids.Request, "model", route.ID, "tier", effectiveRoute.Tier, "error", retryErr, "retry_status", retryStatus)
		fallback := *resp
		fallback.Body = io.NopCloser(bytes.NewReader(errBody))
		return &fallback, effectiveRoute, nil
	}
	return retryResp, retryRoute, nil
}

// isStaleReasoningReference reports whether an upstream 400 body describes a
// reasoning item/reference the server no longer recognizes, such as
// "Referenced reasoning item 'rs_...' was not found or has expired".
// Generic validation errors that merely mention reasoning (e.g. "unknown
// reasoning field") must NOT match, so both the target phrase and the
// gone/expired marker are required.
func isStaleReasoningReference(body []byte) bool {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "reasoning item") && !strings.Contains(text, "reasoning reference") {
		return false
	}
	for _, marker := range []string{"not found", "expir", "does not exist", "no longer"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// stripStaleReasoningInputs removes server-issued reasoning references from
// Responses-protocol upstream payloads: replayed "reasoning" input items and
// any previous_response_id chain link. Other tiers/protocols are passed
// through untouched. It reports whether any payload actually changed.
func stripStaleReasoningInputs(route models.Route, bodies map[config.Tier][]byte) (map[config.Tier][]byte, bool) {
	changed := false
	out := make(map[config.Tier][]byte, len(bodies))
	for tier, body := range bodies {
		if len(body) == 0 || route.ProtocolFor(tier) != wire.Responses {
			out[tier] = body
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			out[tier] = body
			continue
		}
		tierChanged := false
		if _, ok := payload["previous_response_id"]; ok {
			delete(payload, "previous_response_id")
			tierChanged = true
		}
		if raw, ok := payload["input"].([]any); ok {
			kept := make([]any, 0, len(raw))
			for _, item := range raw {
				if m, ok := item.(map[string]any); ok && jsonutil.StringAt(m, "type") == "reasoning" {
					tierChanged = true
					continue
				}
				kept = append(kept, item)
			}
			if tierChanged {
				payload["input"] = kept
			}
		}
		if !tierChanged {
			out[tier] = body
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			out[tier] = body
			continue
		}
		out[tier] = encoded
		changed = true
	}
	return out, changed
}

func (g *Gateway) doUpstreamTiers(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, models.Route, int, error) {
	var lastResponse *http.Response
	var lastErr error
	effectiveRoute := route
	attempts := attemptOffset
	g.dumpOutboundBodies(route, bodies, ids, attemptOffset)
	if override, selected := debugKeyOverrideFrom(ctx); selected {
		resp, err, used := g.doSelectedKeyUpstream(ctx, route, bodies, ids, override, attemptOffset)
		return resp, route, used, err
	}
	if route.Anonymous {
		resp, err, used := g.doAnonymousUpstream(ctx, route, bodies, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, route, attempts, nil
		}
		lastResponse, lastErr = resp, err
		if len(route.KeyTiers) > 0 {
			g.logger.Debug("anonymous request did not succeed; entering preferred key tiers", "component", "upstream", "event", "anonymous_fallback", "request_id", ids.Request, "attempts", attempts, "key_tiers", route.KeyTiers)
		}
	}
	if ctx.Err() != nil {
		// The anonymous phase already consumed the request budget. Entering the
		// authenticated tiers here would only fire instant attempts against a
		// dead context, cooling keys that were never really tried.
		if lastResponse != nil {
			return lastResponse, effectiveRoute, attempts, nil
		}
		if lastErr == nil {
			lastErr = ctx.Err()
		}
		return nil, effectiveRoute, attempts, lastErr
	}

	keyTiers := route.KeyTiers
	if !route.Anonymous && len(keyTiers) == 0 && (route.Tier == config.TierZen || route.Tier == config.TierGo) {
		keyTiers = []config.Tier{route.Tier}
	}
	for _, tier := range keyTiers {
		if lastResponse != nil {
			httpx.DrainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		keyRoute := route
		keyRoute.Tier = tier
		keyRoute.Anonymous = false
		keyRoute.Protocol = route.ProtocolFor(tier)
		effectiveRoute = keyRoute
		resp, err, used := g.doKeyUpstream(ctx, keyRoute, bodies, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, keyRoute, attempts, nil
		}
		lastResponse, lastErr = resp, err
	}
	if lastResponse != nil {
		return lastResponse, effectiveRoute, attempts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable upstream route")
	}
	return nil, effectiveRoute, attempts, lastErr
}

// doAnonymousUpstream tries every currently available proxy at most once. Any
// failure, including an HTTP error response, advances to the next proxy. Only a
// successful response ends the anonymous phase; exhausting the proxy cursor
// returns control to the preferred authenticated tiers.
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	cursor := g.anonymous.CursorFor(ids.Session)
	limit := g.anonymous.Len()
	attempts := 0
	body := bodies[config.TierZen]
	if len(body) == 0 {
		return nil, errors.New("no prepared Zen request body"), 0
	}
	// The anonymous free tier only serves agent-shaped streaming requests
	// (anything else is rejected with 403 FreeTierError). Normalize the
	// wire body here; the gateway collapses the stream back when the
	// downstream client asked for a plain JSON reply. Key tiers keep their
	// original bodies.
	body = prepareAnonymousBody(body, route.ProtocolFor(config.TierZen))
	for attempts < limit {
		if ctx.Err() != nil {
			// The request-level budget is already gone. Stop scanning instead
			// of firing attempts that cannot succeed and then recording them
			// as failures against proxies that were never really tried.
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		if meta := telemetry.MetaFromContext(ctx); meta != nil {
			meta.Attempts = attemptOffset + attempts
			meta.Tier = string(config.TierZen)
		}
		if lastResponse != nil {
			httpx.DrainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		req, err := newUpstreamRequest(ctx, g.cfg.Upstream.Zen, route.Protocol, body, ids, anonymousZenKey)
		if err != nil {
			return nil, err, attempts
		}
		setRequestCredential(ctx, config.TierZen, "anonymous", "anonymous", true, node.proxy)
		started := time.Now()
		resp, err := node.proxy.client.Do(req)
		duration := time.Since(started)
		if ctx.Err() != nil {
			// The parent budget expired while this attempt was in flight. Its
			// outcome says nothing about this proxy, so record nothing and stop
			// scanning.
			lastResponse, lastErr = resp, err
			if lastErr == nil && lastResponse == nil {
				lastErr = ctx.Err()
			}
			break
		}
		g.observeAnonymousResult(ctx, node, resp, err)
		g.recordUpstreamAttempt(ctx, route, ids, attemptOffset+attempts, "anonymous", "anonymous", true, node.proxy, resp, err, duration)
		if err == nil && resp.StatusCode/100 == 2 {
			g.logger.Debug("anonymous upstream accepted request", "component", "upstream", "event", "anonymous_attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
			return resp, nil, attempts
		}
		lastResponse = resp
		lastErr = err
		if err != nil {
			g.logger.Debug("anonymous transport attempt failed", "component", "upstream", "event", "anonymous_attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "duration_ms", duration.Milliseconds(), "error", err)
		} else {
			g.logger.Debug("anonymous upstream returned an error response; trying the next proxy", "component", "upstream", "event", "anonymous_attempt_response_failed", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
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

// anonymousCoreTools are the tool names the anonymous free tier expects on
// an agent-shaped request. Requests without them are rejected with 403
// FreeTierError. Only the names matter; the gateway synthesizes minimal
// definitions for whichever ones the downstream client did not declare.
var anonymousCoreTools = []string{"bash", "edit", "glob", "grep", "read"}

// prepareAnonymousBody returns a copy of body normalized for the anonymous
// free tier: streaming enabled plus the core agent tools present. Bodies
// that already satisfy both (or are not JSON objects) are returned
// unchanged.
func prepareAnonymousBody(body []byte, protocol wire.Protocol) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	if streaming, ok := payload["stream"].(bool); !ok || !streaming {
		payload["stream"] = true
		changed = true
	}
	if ensureAnonymousTools(payload, protocol) {
		changed = true
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// ensureAnonymousTools appends minimal definitions for any missing core
// tool so the request reads as an agent session upstream. It reports
// whether the payload changed. Tools the client already declared are left
// untouched.
func ensureAnonymousTools(payload map[string]any, protocol wire.Protocol) bool {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = anonymousToolset(protocol, nil)
		return true
	}
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	present := make(map[string]bool, len(items))
	for _, item := range items {
		if name := anonymousToolName(protocol, item); name != "" {
			present[name] = true
		}
	}
	missing := make([]string, 0, len(anonymousCoreTools))
	for _, name := range anonymousCoreTools {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return false
	}
	payload["tools"] = append(items, anonymousToolset(protocol, missing)...)
	return true
}

func anonymousToolName(protocol wire.Protocol, item any) string {
	entry, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if protocol == wire.Chat {
		return jsonutil.StringAt(entry, "function", "name")
	}
	return jsonutil.StringAt(entry, "name")
}

func anonymousToolset(protocol wire.Protocol, names []string) []any {
	if names == nil {
		names = anonymousCoreTools
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, anonymousTool(protocol, name))
	}
	return tools
}

func anonymousTool(protocol wire.Protocol, name string) map[string]any {
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	switch protocol {
	case wire.Chat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  parameters,
			},
		}
	case wire.Anthropic:
		return map[string]any{
			"name":         name,
			"description":  description,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}
	}
}

// forceStreamBody returns a copy of body with streaming enabled. Bodies
// that already stream (or are not JSON objects) are returned unchanged.
func forceStreamBody(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if streaming, ok := payload["stream"].(bool); ok && streaming {
		return body
	}
	payload["stream"] = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// requestBodyDumpLimit caps how much of an outbound body is written to the log.
const requestBodyDumpLimit = 64 << 10

// dumpOutboundBodies records the exact bytes about to be sent upstream when
// logging.dump_request_bodies is enabled. It exists so retry and translation
// problems (stale rs_* references, re-encoded history, tier re-encoding) can be
// diagnosed without patching the binary. Credentials travel in headers and never
// in the body. The logger redacts configured secrets, but conversation content
// can still be sensitive. Reference IDs are retained for replay diagnostics.
func (g *Gateway) dumpOutboundBodies(route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) {
	if !g.cfg.Logging.DumpRequestBodies || len(bodies) == 0 {
		return
	}
	phase := "initial"
	if attemptOffset > 0 {
		phase = "retry"
	}
	tiers := make([]string, 0, len(bodies))
	for tier := range bodies {
		tiers = append(tiers, string(tier))
	}
	sort.Strings(tiers)
	for _, name := range tiers {
		tier := config.Tier(name)
		body := bodies[tier]
		if len(body) == 0 {
			continue
		}
		sum := sha256.Sum256(body)
		truncated := len(body) > requestBodyDumpLimit
		payload := body
		if truncated {
			payload = body[:requestBodyDumpLimit]
		}
		g.logger.Debug("outbound upstream request body", "component", "upstream", "event", "upstream_request_body",
			"request_id", ids.Request, "model", route.ID, "tier", tier, "protocol", route.ProtocolFor(tier),
			"phase", phase, "bytes", len(body), "sha256", hex.EncodeToString(sum[:8]),
			"truncated", truncated, "body", string(payload))
	}
}

// doSelectedKeyUpstream sends exactly one attempt through the operator-selected
// key. It performs no failover at all — no anonymous lane, no other key and no
// other tier — so the outcome describes that one key. It also leaves pool state
// untouched on purpose: a diagnostic request must never cool a production key or
// evict a proxy, which is exactly what made a bad Playground request degrade
// live traffic before.
func (g *Gateway) doSelectedKeyUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, override DebugKeyOverride, attemptOffset int) (*http.Response, error, int) {
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if override.Tier == config.TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	node := nodes.NodeByID(override.KeyID)
	if node == nil {
		return nil, fmt.Errorf("selected %s key is no longer configured", override.Tier), 0
	}
	body := bodies[override.Tier]
	if len(body) == 0 {
		return nil, fmt.Errorf("no prepared %s request body", override.Tier), 0
	}
	proxy := nodes.Proxy(node)
	if proxy == nil {
		return nil, errors.New("selected upstream key has no proxy binding"), 0
	}
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.Attempts = attemptOffset + 1
		meta.Tier = string(override.Tier)
	}
	keyID := config.KeyDisplayID(node.key)
	setRequestCredential(ctx, override.Tier, keyID, "key", false, proxy)
	req, err := newUpstreamRequest(ctx, baseURL, route.ProtocolFor(override.Tier), body, ids, node.key)
	if err != nil {
		return nil, err, 0
	}
	started := time.Now()
	resp, err := proxy.client.Do(req)
	duration := time.Since(started)
	g.recordUpstreamAttempt(ctx, route, ids, attemptOffset+1, keyID, "key", false, proxy, resp, err, duration)
	if err != nil {
		return nil, err, 1
	}
	return resp, nil, 1
}

func (g *Gateway) doKeyUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	nodes := g.zenNodes
	baseURL := g.cfg.Upstream.Zen
	if route.Tier == config.TierGo {
		nodes = g.goNodes
		baseURL = g.cfg.Upstream.Go
	}
	cursor := nodes.CursorFor(ids.Session)
	if nodes.Len() == 0 {
		return nil, fmt.Errorf("no %s nodes configured", route.Tier), 0
	}
	attempts := 0
	body := bodies[route.Tier]
	if len(body) == 0 {
		return nil, fmt.Errorf("no prepared %s request body", route.Tier), 0
	}
	for attempts < g.cfg.Retry.MaxAttempts {
		if ctx.Err() != nil {
			// Same guard as the anonymous loop: an exhausted request budget
			// must not be spent rotating keys that were never really tried.
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		// Keep the request-level trace synchronized with the attempt that is
		// about to be sent. Only the redacted key suffix is retained.
		if meta := telemetry.MetaFromContext(ctx); meta != nil {
			meta.Attempts = attemptOffset + attempts
			meta.Tier = string(route.Tier)
		}
		if lastResponse != nil {
			httpx.DrainAndClose(lastResponse.Body)
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
		keyID := config.KeyDisplayID(node.key)
		setRequestCredential(ctx, route.Tier, keyID, "key", false, proxy)
		attemptStarted := time.Now()
		resp, err := proxy.client.Do(req)
		attemptDuration := time.Since(attemptStarted)
		if ctx.Err() != nil {
			// The request budget expired while this attempt was in flight. A
			// cancelled context says nothing about the key or the proxy: without
			// this guard the instant DeadlineExceeded would cool a healthy key
			// and evict a healthy proxy from the pool.
			lastResponse, lastErr = resp, err
			if lastErr == nil && lastResponse == nil {
				lastErr = ctx.Err()
			}
			break
		}
		g.observeKeyResult(ctx, nodes, node, proxy, resp, err)
		g.recordUpstreamAttempt(ctx, route, ids, attemptOffset+attempts, keyID, "key", false, proxy, resp, err, attemptDuration)
		if err == nil && resp.StatusCode/100 == 2 {
			g.logger.Debug("upstream accepted request", "component", "upstream", "event", "attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", keyID, "proxy", config.RedactURL(proxy.name), "status", resp.StatusCode, "duration_ms", attemptDuration.Milliseconds())
			return resp, nil, attempts
		}
		// Request-shape errors are deterministic and must leave this tier without
		// rotating through unrelated keys. The outer route may still try the next
		// tier in prefer order. Authentication, throttling, server, and transport
		// failures remain retryable inside this tier.
		if isNonRetryableClientResponse(resp, err) {
			g.logger.Debug("upstream rejected a non-retryable request", "component", "upstream", "event", "attempt_rejected", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", keyID, "status", resp.StatusCode, "proxy", config.RedactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds())
			return resp, nil, attempts
		}
		lastResponse = resp
		lastErr = err
		if err != nil {
			g.logger.Debug("upstream transport attempt failed", "component", "upstream", "event", "attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", keyID, "proxy", config.RedactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds(), "error", err)
		} else {
			g.logger.Debug("upstream returned a retryable response", "component", "upstream", "event", "attempt_retryable_response", "request_id", ids.Request, "attempt", attempts, "tier", route.Tier, "key_id", keyID, "status", resp.StatusCode, "proxy", config.RedactURL(proxy.name), "duration_ms", attemptDuration.Milliseconds())
		}
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	return nil, lastErr, attempts
}

// Diagnostics use the same routing and transports as production, but must not
// update cooldowns, health, or bindings, including after successful attempts.
func (g *Gateway) observeKeyResult(ctx context.Context, nodes *nodePool, node *upstreamNode, proxy *proxyTransport, resp *http.Response, err error) {
	if isDiagnosticRequest(ctx) {
		return
	}
	status := upstreamStatus(resp)
	proxyFailed := g.syncProxyResult(ctx, proxy, status, err)
	if err == nil && status/100 == 2 || isNonRetryableClientResponse(resp, err) {
		nodes.MarkSuccess(node)
		return
	}
	if !proxyFailed || nodes.Proxy(node) == proxy {
		nodes.MarkFailure(node, resp, err)
	}
}

func (g *Gateway) observeAnonymousResult(ctx context.Context, node *anonymousNode, resp *http.Response, err error) {
	if isDiagnosticRequest(ctx) {
		return
	}
	status := upstreamStatus(resp)
	g.syncProxyResult(ctx, node.proxy, status, err)
	if err == nil && status/100 == 2 {
		g.anonymous.MarkSuccess(node)
	} else {
		g.anonymous.MarkFailure(node, resp, err)
	}
}

func upstreamStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func setRequestCredential(ctx context.Context, tier config.Tier, keyID, channel string, anonymous bool, proxy *proxyTransport) {
	meta := telemetry.MetaFromContext(ctx)
	if meta == nil {
		return
	}
	meta.Tier = string(tier)
	meta.KeyID = keyID
	meta.Channel = channel
	meta.Anonymous = anonymous
	meta.Proxy = ""
	if proxy != nil {
		meta.Proxy = config.RedactURL(proxy.name)
	}
}

func requestCredential(ctx context.Context) (string, string, bool) {
	meta := telemetry.MetaFromContext(ctx)
	if meta == nil {
		return "", "", false
	}
	return meta.KeyID, meta.Channel, meta.Anonymous
}

func (g *Gateway) recordUpstreamAttempt(ctx context.Context, route models.Route, ids identity.RequestIDs, attempt int, keyID, channel string, anonymous bool, proxy *proxyTransport, resp *http.Response, err error, duration time.Duration) {
	status := upstreamStatus(resp)
	success := err == nil && status >= 200 && status < 300
	outcome := "retryable_failure"
	if success {
		outcome = "success"
	} else if err != nil {
		outcome = "transport_error"
	} else if isNonRetryableClientResponse(resp, nil) {
		outcome = "rejected"
	}
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.AttemptOutcome = outcome
		meta.Protocol = route.Protocol
		// Attempt ledger: key:status:ms per leg on the request line.
		label := keyID
		if anonymous {
			label = "anon"
		}
		leg := label + ":"
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				leg += "timeout"
			} else {
				leg += "transport"
			}
		} else {
			leg += strconv.Itoa(status)
		}
		leg += ":" + strconv.FormatInt(max(duration.Milliseconds(), 0), 10) + "ms"
		meta.Legs = append(meta.Legs, leg)
	}
	if g.monitor == nil {
		return
	}
	proxyName := "unavailable"
	if proxy != nil {
		proxyName = config.RedactURL(proxy.name)
	}
	g.monitor.RecordAttempt(telemetry.UpstreamAttempt{
		Time: time.Now().UTC(), RequestID: ids.Request, Model: route.ID, Tier: string(route.Tier), Attempt: attempt,
		KeyID: keyID, Channel: channel, Anonymous: anonymous, Proxy: proxyName, Status: status,
		DurationMS: max(duration.Milliseconds(), 0), Success: success, Outcome: outcome,
	})
}

func newUpstreamRequest(ctx context.Context, baseURL string, protocol wire.Protocol, body []byte, ids identity.RequestIDs, key string) (*http.Request, error) {
	endpoint := strings.TrimRight(baseURL, "/") + wire.Path(protocol)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", httpx.UserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", ids.Session)
	// OpenCode 1.18.x sends these correlation headers to preserve provider-side
	// prompt/session affinity. Keep the legacy x-opencode-session header too so
	// older Zen deployments continue to recognize the request.
	req.Header.Set("x-session-affinity", ids.Session)
	req.Header.Set("X-Session-Id", ids.Session)
	req.Header.Set("x-opencode-request", ids.Request)
	req.Header.Set("x-opencode-project", ids.Project)
	if ids.ParentSession != "" {
		req.Header.Set("x-parent-session-id", ids.ParentSession)
	}
	if protocol == wire.Anthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func isNonRetryableClientResponse(resp *http.Response, err error) bool {
	return err == nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests
}
