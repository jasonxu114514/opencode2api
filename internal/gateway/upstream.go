package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// WithDiagnosticRequest marks ctx as a Playground diagnostic request. It uses
// the production routing and transports, but its outcomes never update key
// cooldowns, proxy health or bindings, and its traffic is accounted to the
// Playground server key rather than the key that authenticated it.
func WithDiagnosticRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, diagnosticContextKey{}, true)
}

func isDiagnosticRequest(ctx context.Context) bool {
	diagnostic, _ := ctx.Value(diagnosticContextKey{}).(bool)
	return diagnostic
}

// WithDebugKeyOverride marks ctx as a diagnostic request (see
// WithDiagnosticRequest) pinned to override: the request is routed to
// override.Tier only and sent once through that key, with no failover (see
// doSelectedKeyUpstream). An override without a KeyID is ignored.
func WithDebugKeyOverride(ctx context.Context, override DebugKeyOverride) context.Context {
	return context.WithValue(WithDiagnosticRequest(ctx), debugKeyOverrideContextKey{}, override)
}

func debugKeyOverrideFrom(ctx context.Context) (DebugKeyOverride, bool) {
	override, ok := ctx.Value(debugKeyOverrideContextKey{}).(DebugKeyOverride)
	return override, ok && override.KeyID != ""
}

// doUpstream sends one inference request through the anonymous and key tiers
// (see doUpstreamTiers) and returns the response, whose body the caller must
// close, together with the route that produced it. It repairs one known
// failure: when the returned response is a 400 reporting a stale reasoning
// reference, it replays the request once with those references stripped, and
// returns the original 400 if the replay does not succeed. A 400 body is
// buffered (up to 1 MiB) so it can be read again. Debug key overrides skip the
// replay so the Playground sees the selected key's own answer.
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
	// The network body is always drained and closed here so its connection
	// can be reused, for instance by the retry below; downstream receives a
	// fresh in-memory reader over the cached bytes instead.
	httpx.DrainAndClose(origBody)
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, effectiveRoute, nil
	}
	// Restore the body so downstream error handling still sees the original
	// payload when no retry happens below.
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
	// Log a capped snippet of the upstream error body so the 400 class is
	// visible without dumping full payloads. Bodies stay out of attempt
	// records, and the snippet is scrubbed of configured secrets before it is
	// logged.
	if len(errBody) > 0 {
		redactor := config.SecretRedactor()
		redactor.Replace(g.cfg)
		snippet := redactor.String(string(errBody))
		if len(snippet) > 240 {
			snippet = snippet[:240]
		}
		g.logger.Info("upstream error returned or retried", "component", "upstream", "event", "upstream_error_body", "request_id", ids.Request, "model", route.ID, "status", resp.StatusCode, "snippet", snippet)
	}
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

// doUpstreamTiers runs one routing pass and reports the response, the route
// that produced it, and the attempt count so far (attemptOffset plus the
// attempts made here). A debug key override short-circuits to that single key
// and reports its attempt including attemptOffset. Otherwise an anonymous
// route first tries the free tier, then every tier in route.KeyTiers order (a
// keyed Zen or Go route without KeyTiers uses its own tier) until one returns
// 2xx. Without a success it returns the last HTTP response, whose body is still
// open, in preference to the last error. Once ctx is done it no longer enters
// the key tiers.
func (g *Gateway) doUpstreamTiers(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, models.Route, int, error) {
	var lastResponse *http.Response
	var lastErr error
	effectiveRoute := route
	attempts := attemptOffset
	g.dumpOutboundBodies(route, bodies, ids, attemptOffset)
	if override, selected := debugKeyOverrideFrom(ctx); selected {
		resp, err, used := g.doSelectedKeyUpstream(ctx, route, bodies, ids, override, attemptOffset)
		return resp, route, attemptOffset + used, err
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
		keyRoute := route
		keyRoute.Tier = tier
		keyRoute.Anonymous = false
		keyRoute.Protocol = route.ProtocolFor(tier)
		effectiveRoute = keyRoute
		resp, err, used := g.doKeyUpstream(ctx, keyRoute, bodies, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			if lastResponse != nil {
				httpx.DrainAndClose(lastResponse.Body)
			}
			return resp, keyRoute, attempts, nil
		}
		if resp != nil {
			if lastResponse != nil {
				httpx.DrainAndClose(lastResponse.Body)
			}
			lastResponse = resp
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastResponse != nil {
		return lastResponse, effectiveRoute, attempts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable upstream route")
	}
	return nil, effectiveRoute, attempts, lastErr
}

// doAnonymousUpstream walks the session's anonymous proxy cursor. Each static
// proxy is tried at most once; rotating #pool proxies may be drawn again until
// Retry.MaxAttempts proxies were used. For each proxy it tries the upstreams in
// anonymousUpstreams order: the v1 lane (models.V1Base) before the v2
// /inference upstream (Upstream.Anonymous). Any failure, including an HTTP
// error response, advances to the next upstream or proxy; a proxy-route
// failure, or a proxy marked unhealthy, skips that proxy's remaining
// upstreams. Only a successful response ends the anonymous phase; exhausting
// the proxy cursor returns control to the preferred authenticated tiers. It
// returns the last response (body still open), else the last error, plus the
// number of attempts made.
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	cursor := g.anonymous.CursorFor(ids.Session)
	limit := g.anonymous.Len()
	if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
		limit = g.cfg.Retry.MaxAttempts
	}
	proxies := 0
	attempts := 0
	rawBody := bodies[config.TierZen]
	if len(rawBody) == 0 {
		return nil, errors.New("no prepared Zen request body"), 0
	}
	// The anonymous free tier only serves agent-shaped streaming requests
	// carrying the required tool definitions (anything else is rejected with 403
	// FreeTierError). Normalize the wire body per upstream gate (v1 /zen vs v2
	// /inference); the gateway collapses the stream back when the downstream
	// client asked for a plain JSON reply. Key tiers shape their own bodies
	// (see shapeKeyBody).
	protocol := route.ProtocolFor(config.TierZen)
	upstreams := g.anonymousUpstreams()
	preparedBodies := make([][]byte, len(upstreams))
	toolAliases := make([]string, len(upstreams))
	for i, baseURL := range upstreams {
		preparedBodies[i], toolAliases[i] = prepareAnonymousBody(rawBody, protocol, models.IsV1Upstream(baseURL))
	}
	for proxies < limit {
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
		proxies++
		var nodeResp *http.Response
		var nodeErr error
		for index, baseURL := range upstreams {
			if ctx.Err() != nil {
				if lastErr == nil && lastResponse == nil {
					lastErr = ctx.Err()
				}
				break
			}
			setRequestShape(ctx, false, toolAliases[index])
			if meta := telemetry.MetaFromContext(ctx); meta != nil {
				meta.Attempts = attemptOffset + attempts
				meta.Tier = string(config.TierZen)
			}
			if lastResponse != nil {
				httpx.DrainAndClose(lastResponse.Body)
				lastResponse = nil
			}
			req, err := upstreamRequest(ctx, baseURL, protocol, preparedBodies[index], ids, anonymousZenKey)
			if err != nil {
				return nil, err, attempts
			}
			if node.proxy.pool {
				req.Close = true
			}
			setRequestCredential(ctx, config.TierZen, "anonymous", "anonymous", true, node.proxy)
			started := time.Now()
			resp, err := doInferenceAttempt(g.inferenceClient(node.proxy, baseURL, route.ID), req, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second, g.attemptTimeout)
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
			nodeResp, nodeErr = resp, err
			g.recordUpstreamAttempt(ctx, route, ids, attemptOffset+attempts, "anonymous", "anonymous", true, node.proxy, resp, err, duration)
			if err == nil && resp.StatusCode/100 == 2 {
				g.observeAnonymousResult(ctx, node, resp, nil)
				g.logger.Debug("anonymous upstream accepted request", "component", "upstream", "event", "anonymous_attempt_succeeded", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "upstream", config.RedactURL(baseURL), "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
				return resp, nil, attempts
			}
			lastResponse = resp
			lastErr = asProxyUnavailable(node.proxy, err)
			if err != nil {
				g.logger.Debug("anonymous transport attempt failed", "component", "upstream", "event", "anonymous_attempt_transport_failed", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "upstream", config.RedactURL(baseURL), "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "duration_ms", duration.Milliseconds(), "error", err)
				if isProxyFailure(err) {
					break
				}
			} else {
				g.logger.Debug("anonymous upstream returned an error response; trying the next upstream or proxy", "component", "upstream", "event", "anonymous_attempt_response_failed", "request_id", ids.Request, "attempt", attempts, "tier", config.TierZen, "upstream", config.RedactURL(baseURL), "key_id", "anonymous", "channel", "anonymous", "anonymous", true, "proxy", config.RedactURL(node.proxy.name), "status", resp.StatusCode, "duration_ms", duration.Milliseconds())
			}
			if index+1 < len(upstreams) && !node.proxy.healthy.Load() {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		g.observeAnonymousResult(ctx, node, nodeResp, nodeErr)
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy anonymous proxies available")
		if !g.transports.hasHealthy() {
			lastErr = fmt.Errorf("no healthy anonymous proxies available: %w", errProxyUnavailable)
		}
	}
	return nil, lastErr, attempts
}

// anonymousUpstreams returns the ordered base URLs for the keyless free tier:
// the v1 origin (models.V1Base) is tried first, and the configured v2
// /inference origin (Upstream.Anonymous) is kept as a fallback. Only the v1
// origin is returned when Upstream.Anonymous is empty or names that same
// origin.
func (g *Gateway) anonymousUpstreams() []string {
	v1 := strings.TrimRight(models.V1Base, "/")
	primary := strings.TrimRight(g.cfg.Upstream.Anonymous, "/")
	if primary == "" || strings.EqualFold(v1, primary) {
		return []string{models.V1Base}
	}
	return []string{g.cfg.Upstream.Anonymous, models.V1Base}
}

// inferenceClient picks the client of proxy for a request for model to
// baseURL. Free-model traffic to the /zen and /inference lanes goes through
// the Bun client (see httpx.BunTransport), whose TLS ClientHello and HTTP/1.1
// header layout match the runtime of the official OpenCode clients; every
// other request, including the Go tier (/zen/go), keeps the regular client.
func (g *Gateway) inferenceClient(proxy *proxyTransport, baseURL, model string) *http.Client {
	lane := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if g.catalog.IsFreeModel(model) && (models.IsV1Upstream(baseURL) || strings.HasSuffix(lane, "/inference")) {
		return proxy.bun
	}
	return proxy.client
}

// v1ReadTool is the single tool name required by the v1 free-tier gate
// (/zen). Unlike the v2 /inference gate, it does not require a
// shell/edit/glob/grep toolset and does not validate tool descriptions or
// parameter schemas: a single tool named "read" is sufficient.
const v1ReadTool = "read"

// v1DeprecatedReadDescription is attached when the gateway must inject a
// placeholder "read" tool on the v1 lane (/zen) because the client request
// declared no read-related tool of its own. It explicitly tells the model that
// the tool is deprecated and must never be invoked.
const v1DeprecatedReadDescription = "DEPRECATED: This tool is deprecated and disabled. Do NOT call or reference this tool under any circumstances; ignore it completely."

// v2CoreTools are the tool names the v2 /inference free-tier gate expects on
// an agent-shaped request. Requests without a shell tool and a read tool are
// rejected with 403 FreeTierError.
//
// "shell" is the name OpenCode 2.x ships; the v1 client's "bash" spelling is
// deliberately not injected on /inference because that gate validates the
// definition of every tool it recognises, and a "bash" definition the gateway
// synthesises does not match the one upstream expects. Since 2026-10-14 name
// presence is not enough on /inference either: the upstream compares each
// recognised tool's description and parameter schema against the official CLI's
// definitions byte-for-byte (minimal stand-ins 403), which is why the genuine
// definitions below are embedded. Tool order is irrelevant and extra client
// tools are allowed.
var v2CoreTools = []string{"shell", "edit", "glob", "grep", "read"}

// opencodeToolsJSON holds the genuine OpenCode tool definitions (description
// and JSON schema) captured from the official client. The /inference free-tier
// gate checks the definition of every tool whose name it recognises, so a
// synthesised stub — even one carrying the right name — is rejected with 403
// FreeTierError. The gateway therefore ships the real definitions for the v2
// lane and uses them both for the tools it injects and for any same-named tool
// the downstream client declared.
//
//go:embed opencode_tools.json
var opencodeToolsJSON []byte

// opencodeToolDefinition is one entry of opencode_tools.json.
type opencodeToolDefinition struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	opencodeToolsOnce sync.Once
	opencodeTools     map[string]opencodeToolDefinition
)

// knownOpencodeTool returns the genuine definition for an OpenCode tool name.
// The embedded JSON is decoded once; if it fails to decode, no tool is known.
func knownOpencodeTool(name string) (opencodeToolDefinition, bool) {
	opencodeToolsOnce.Do(func() {
		definitions := map[string]opencodeToolDefinition{}
		if err := json.Unmarshal(opencodeToolsJSON, &definitions); err != nil {
			definitions = map[string]opencodeToolDefinition{}
		}
		opencodeTools = definitions
	})
	definition, ok := opencodeTools[name]
	return definition, ok
}

// prepareAnonymousBody returns body normalized for the free tier alongside any
// original client tool name that was aliased to "read" for the v1 gate. The
// body is forced to stream (keeping Chat usage reporting on) and its tools are
// shaped for the lane v1 selects; an unchanged body is returned as is.
//
// On the v1 gate (v1 == true), none of the v2 core tools are injected: if the
// client declared a read-related tool (case-insensitive, e.g. "reAd",
// "Read_file", "readFile"), it is renamed in-place to "read" while keeping its
// original description and parameters, and its original name is returned so
// the gateway can restore it in the downstream response; if no read-related
// tool is present, a single deprecated "read" stub is injected.
//
// On a v2 gate (v1 == false), v2CoreTools with genuine CLI 2.x definitions are
// ensured. System One payloads are decision requests, not agent traffic, so
// they are forwarded verbatim, as is a body that is not a JSON object.
func prepareAnonymousBody(body []byte, protocol wire.Protocol, v1 bool) ([]byte, string) {
	if protocol == wire.SystemOne {
		return body, ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, ""
	}
	changed := false
	if streaming, ok := payload["stream"].(bool); !ok || !streaming {
		payload["stream"] = true
		changed = true
	}
	if ensureAnonymousChatUsage(payload, protocol) {
		changed = true
	}
	var alias string
	if v1 {
		var toolChanged bool
		alias, toolChanged = ensureV1ReadTool(payload, protocol)
		if toolChanged {
			changed = true
		}
	} else if ensureV2Tools(payload, protocol) {
		changed = true
	}
	if !changed {
		return body, alias
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// The alias is dropped with the failed re-encode: the body never left
		// the gateway, so there is nothing downstream to restore.
		return body, ""
	}
	return encoded, alias
}

// ensureAnonymousChatUsage keeps token usage available when a non-streaming
// request is forced onto the Chat SSE path. OpenAI-compatible Chat streams
// require stream_options.include_usage for the final usage event. It reports
// whether the payload changed.
func ensureAnonymousChatUsage(payload map[string]any, protocol wire.Protocol) bool {
	if protocol != wire.Chat {
		return false
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok {
		payload["stream_options"] = map[string]any{"include_usage": true}
		return true
	}
	includeUsage, ok := options["include_usage"].(bool)
	if ok && includeUsage {
		return false
	}
	options["include_usage"] = true
	return true
}

// ensureV1ReadTool satisfies the v1 free-tier gate (/zen) using only the
// single "read" tool requirement:
//  1. If the client already has an exact "read" tool, nothing is injected or
//     renamed.
//  2. If the client has a read-related tool (case-insensitive match such as
//     "reAd", "read_file", "ReadFile", etc.), that tool is renamed to "read"
//     (preserving its client description and schema), any matching tool_choice
//     or historical tool call in the request is rewritten to "read", and the
//     client's original tool name is returned so responses can be restored.
//     A case-insensitive "read" is preferred over other read-related names.
//  3. If the client has no read-related tool at all, a single "read" stub tool
//     is appended with a deprecated/ignore description so the model never
//     invokes it.
//
// It also reports whether the payload changed. A "tools" value that is not an
// array is left untouched.
func ensureV1ReadTool(payload map[string]any, protocol wire.Protocol) (string, bool) {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = []any{v1DeprecatedReadTool(protocol)}
		return "", true
	}
	items, ok := raw.([]any)
	if !ok {
		return "", false
	}
	candidateIndex := -1
	candidateName := ""
	caseFoldCandidate := false
	for index, item := range items {
		name := anonymousToolName(protocol, item)
		if name == "" {
			continue
		}
		if name == v1ReadTool {
			return "", false
		}
		if strings.EqualFold(name, v1ReadTool) {
			if !caseFoldCandidate {
				candidateIndex = index
				candidateName = name
				caseFoldCandidate = true
			}
			continue
		}
		if candidateIndex < 0 && isV1ReadToolCandidate(name) {
			candidateIndex = index
			candidateName = name
		}
	}
	if candidateIndex >= 0 {
		setAnonymousToolName(protocol, items[candidateIndex], v1ReadTool)
		rewriteV1ToolReferences(payload, protocol, candidateName, v1ReadTool)
		payload["tools"] = items
		return candidateName, true
	}
	items = append(items, v1DeprecatedReadTool(protocol))
	payload["tools"] = items
	return "", true
}

// isV1ReadToolCandidate reports whether a client tool name represents a
// read-related tool (case-insensitive), such as "reAd", "read_file",
// "ReadFile", "file_read", or "view_file".
func isV1ReadToolCandidate(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if lower == "read" {
		return true
	}
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)
	switch normalized {
	case "readfile", "fileread", "readfiles", "readtext", "readtextfile", "readcontent", "viewfile":
		return true
	}
	return strings.HasPrefix(lower, "read_") || strings.HasPrefix(lower, "read-") || strings.HasSuffix(lower, "_read") || strings.HasSuffix(lower, "-read")
}

// setAnonymousToolName updates the function name of a tool definition in-place
// while keeping all other client fields (description, parameters/input_schema,
// strict, etc.) intact.
func setAnonymousToolName(protocol wire.Protocol, item any, name string) {
	entry, ok := item.(map[string]any)
	if !ok {
		return
	}
	if protocol == wire.Chat {
		if function, ok := entry["function"].(map[string]any); ok {
			function["name"] = name
		}
		return
	}
	entry["name"] = name
}

// rewriteV1ToolReferences renames any named tool_choice and
// assistant tool-call history entries from fromName to toName so the outbound
// request stays self-consistent after a client read tool is aliased to "read".
func rewriteV1ToolReferences(payload map[string]any, protocol wire.Protocol, fromName, toName string) {
	if fromName == "" || fromName == toName {
		return
	}
	switch protocol {
	case wire.Chat:
		if choice, ok := payload["tool_choice"].(map[string]any); ok {
			if function, ok := choice["function"].(map[string]any); ok && jsonutil.StringAt(function, "name") == fromName {
				function["name"] = toName
			}
		}
		for _, rawMsg := range jsonutil.SliceAt(payload, "messages") {
			msg, _ := rawMsg.(map[string]any)
			for _, rawCall := range jsonutil.SliceAt(msg, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				if function, ok := call["function"].(map[string]any); ok && jsonutil.StringAt(function, "name") == fromName {
					function["name"] = toName
				}
			}
		}
	case wire.Responses:
		if choice, ok := payload["tool_choice"].(map[string]any); ok && jsonutil.StringAt(choice, "name") == fromName {
			choice["name"] = toName
		}
		for _, rawItem := range jsonutil.SliceAt(payload, "input") {
			item, _ := rawItem.(map[string]any)
			if jsonutil.StringAt(item, "type") == "function_call" && jsonutil.StringAt(item, "name") == fromName {
				item["name"] = toName
			}
		}
	case wire.Anthropic:
		if choice, ok := payload["tool_choice"].(map[string]any); ok && jsonutil.StringAt(choice, "name") == fromName {
			choice["name"] = toName
		}
		for _, rawMsg := range jsonutil.SliceAt(payload, "messages") {
			msg, _ := rawMsg.(map[string]any)
			for _, rawBlock := range jsonutil.SliceAt(msg, "content") {
				block, _ := rawBlock.(map[string]any)
				if jsonutil.StringAt(block, "type") == "tool_use" && jsonutil.StringAt(block, "name") == fromName {
					block["name"] = toName
				}
			}
		}
	}
}

// v1DeprecatedReadTool builds the single placeholder "read" tool injected on
// the v1 lane (/zen) when the client request has no read-related tool of its
// own, in the wire shape of protocol. Its description marks the tool as
// deprecated and instructs the model to ignore it completely.
func v1DeprecatedReadTool(protocol wire.Protocol) map[string]any {
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	switch protocol {
	case wire.Chat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        v1ReadTool,
				"description": v1DeprecatedReadDescription,
				"parameters":  parameters,
			},
		}
	case wire.Anthropic:
		return map[string]any{
			"name":         v1ReadTool,
			"description":  v1DeprecatedReadDescription,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        v1ReadTool,
			"description": v1DeprecatedReadDescription,
			"parameters":  parameters,
		}
	}
}

// ensureV2Tools makes the tool list read as an OpenCode 2.x agent session on
// a v2 gate: every recognised OpenCode tool carries its genuine CLI 2.x
// definition and missing core tools are appended with genuine schemas;
// unrecognised client tools are left untouched. It reports whether the payload
// changed; a "tools" value that is not an array is left untouched.
func ensureV2Tools(payload map[string]any, protocol wire.Protocol) bool {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = v2Toolset(protocol, v2CoreTools)
		return true
	}
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	changed := false
	present := make(map[string]bool, len(items))
	for index, item := range items {
		name := anonymousToolName(protocol, item)
		if name == "" {
			continue
		}
		present[name] = true
		if _, known := knownOpencodeTool(name); known {
			items[index] = v2Tool(protocol, name)
			changed = true
		}
	}
	missing := make([]string, 0, len(v2CoreTools))
	for _, name := range v2CoreTools {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		items = append(items, v2Toolset(protocol, missing)...)
		changed = true
	}
	if !changed {
		return false
	}
	payload["tools"] = items
	return true
}

// anonymousToolName returns the name of a tool definition in the wire shape
// of protocol (function.name on Chat, name elsewhere), or "" when it has none.
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

// v2Toolset returns the wire-shaped genuine CLI 2.x definitions for the given
// tool names on a v2 gate. A nil names selects every v2CoreTools entry.
func v2Toolset(protocol wire.Protocol, names []string) []any {
	if names == nil {
		names = v2CoreTools
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		if tool := v2Tool(protocol, name); tool != nil {
			tools = append(tools, tool)
		}
	}
	return tools
}

// v2Tool renders one tool in the shape the given protocol expects, using the
// genuine OpenCode CLI 2.x definition whenever one is known and an "Agent tool
// <name>" placeholder with an empty object schema otherwise. SystemOne
// payloads never reach this path (prepareAnonymousBody forwards them
// verbatim), so it falls back to the Responses/default shape.
func v2Tool(protocol wire.Protocol, name string) map[string]any {
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	if definition, ok := knownOpencodeTool(name); ok {
		description = definition.Description
		if definition.Parameters != nil {
			parameters = definition.Parameters
		}
	}
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

// shapeKeyBody normalizes key-tier free-model wire bodies to agent shape
// (stream + required tools), mirroring the anonymous lane; the tier's base URL
// decides between v1 and v2 shaping. Upstream now rejects non-agent-shaped
// free-tier requests on every lane with 403 FreeTierError; paid models keep
// their original bodies. It reports the shaped body, any v1 "read" tool alias
// that must be restored in the response, and whether the body changed so the
// gateway can collapse the SSE stream a non-streaming client receives back.
func (g *Gateway) shapeKeyBody(body []byte, route models.Route, tier config.Tier) ([]byte, string, bool) {
	if !g.catalog.IsFreeModel(route.ID) {
		return body, "", false
	}
	baseURL := g.cfg.Upstream.Zen
	if tier == config.TierGo {
		baseURL = g.cfg.Upstream.Go
	}
	shaped, alias := prepareAnonymousBody(body, route.ProtocolFor(tier), models.IsV1Upstream(baseURL))
	return shaped, alias, !bytes.Equal(shaped, body)
}

// doSelectedKeyUpstream sends exactly one attempt through the operator-selected
// key. It performs no failover at all — no anonymous lane, no other key and no
// other tier — so the outcome describes that one key. It also leaves pool state
// untouched on purpose: a diagnostic request must never cool a production key or
// evict a proxy, which is exactly what made a bad Playground request degrade
// live traffic before. The attempt is still recorded in telemetry, and any HTTP
// response, whatever its status, is returned as is.
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
	shaped, alias, changed := g.shapeKeyBody(body, route, override.Tier)
	if changed {
		body = shaped
	}
	setRequestShape(ctx, changed, alias)
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
	req, err := upstreamRequest(ctx, baseURL, route.ProtocolFor(override.Tier), body, ids, node.key)
	if err != nil {
		return nil, err, 0
	}
	if proxy.pool {
		req.Close = true
	}
	started := time.Now()
	resp, err := doInferenceAttempt(g.inferenceClient(proxy, baseURL, route.ID), req, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second, g.attemptTimeout)
	duration := time.Since(started)
	g.recordUpstreamAttempt(ctx, route, ids, attemptOffset+1, keyID, "key", false, proxy, resp, err, duration)
	if err != nil {
		return nil, asProxyUnavailable(proxy, err), 1
	}
	return resp, nil, 1
}

// doKeyUpstream rotates through the keys of route.Tier, starting from the
// session's affinity key, for at most Retry.MaxAttempts attempts; free-model
// bodies are shaped first (see shapeKeyBody). Every attempt is fed to
// observeKeyResult, which may cool the key or evict its proxy on failure. A
// 2xx ends the loop, and so does a non-retryable 4xx (see
// isNonRetryableClientResponse), which is returned as is so the caller can
// move on to the next tier; after any other response or transport error the
// next key is tried. An attempt that finishes after ctx is done ends the loop
// without being held against the key or the proxy. It returns the last
// response (body still open), else the last error, plus the number of
// attempts made.
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
	shaped, alias, changed := g.shapeKeyBody(body, route, route.Tier)
	if changed {
		body = shaped
	}
	setRequestShape(ctx, changed, alias)
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
		req, err := upstreamRequest(ctx, baseURL, route.Protocol, body, ids, node.key)
		if err != nil {
			return nil, err, attempts
		}
		proxy := nodes.Proxy(node)
		if proxy == nil {
			lastErr = errors.New("upstream key has no proxy binding")
			break
		}
		if proxy.pool {
			req.Close = true
		}
		keyID := config.KeyDisplayID(node.key)
		setRequestCredential(ctx, route.Tier, keyID, "key", false, proxy)
		attemptStarted := time.Now()
		resp, err := doInferenceAttempt(g.inferenceClient(proxy, baseURL, route.ID), req, time.Duration(g.cfg.Performance.FirstEventTimeoutSeconds)*time.Second, g.attemptTimeout)
		attemptDuration := time.Since(attemptStarted)
		if ctx.Err() != nil {
			// The request budget expired while this attempt was in flight. A
			// canceled context says nothing about the key or the proxy: without
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
		lastErr = asProxyUnavailable(proxy, err)
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

// observeKeyResult feeds the outcome of one key attempt back into proxy health
// (syncProxyResult) and the key's cooldown. Diagnostics use the same routing
// and transports as production, but must not update cooldowns, health, or
// bindings, including after successful attempts. A 2xx or non-retryable 4xx
// clears the key's cooldown. Other outcomes go to MarkFailure, which cools the
// key, unless the proxy route itself failed and the key has already been
// rebound to another proxy.
func (g *Gateway) observeKeyResult(ctx context.Context, nodes *upstreamNodes, node *upstreamNode, proxy *proxyTransport, resp *http.Response, err error) {
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

// observeAnonymousResult is the anonymous counterpart of observeKeyResult: it
// syncs the proxy's health and clears or extends the anonymous node's
// per-proxy cooldown, except for diagnostic requests.
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

// setRequestCredential records the credential and proxy of the attempt about
// to be sent on the request's telemetry meta, with the proxy URL redacted. It
// is a no-op when ctx carries no meta.
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

// recordUpstreamAttempt classifies one finished attempt (success, rejected,
// retryable_failure or transport_error), appends its leg to the request's
// telemetry meta and reports it to the metrics monitor when one is configured.
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
		// Append a per-attempt leg (key:status:ms, with "anon" as the key
		// on the anonymous lane) so the request log shows the whole attempt
		// chain at a glance. Only the redacted key suffix is retained.
		label := keyID
		if anonymous {
			label = "anon"
		}
		leg := label + ":"
		if err != nil {
			leg += classifyAttemptError(err)
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

// upstreamRequest builds the POST for one attempt: baseURL plus the protocol's
// /v1/... route, carrying body. The header set follows the lane: setV1Headers
// when models.IsV1Upstream(baseURL), setV2Headers otherwise. key is sent as
// x-api-key (with anthropic-version) on the Anthropic protocol and as a Bearer
// token on every other one; the anonymous lane passes anonymousZenKey.
func upstreamRequest(ctx context.Context, baseURL string, protocol wire.Protocol, body []byte, ids identity.RequestIDs, key string) (*http.Request, error) {
	endpoint := strings.TrimRight(baseURL, "/") + wire.Path(protocol)
	if protocol == wire.Anthropic && !strings.Contains(endpoint, "beta=true") {
		endpoint += "?beta=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if models.IsV1Upstream(baseURL) {
		setV1Headers(req.Header, protocol, body, ids)
	} else {
		setV2Headers(req.Header, protocol, ids)
	}
	if protocol == wire.Anthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		// The anonymous credential is the literal token "public". The 2026-10-14
		// gate rejects keyless requests (an absent header used to pass) and is
		// case-sensitive: "Bearer PUBLIC" 401s.
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

// setV1Headers renders the v1 lane's header set, that of an OpenCode 1.19.3
// model request on its opencode provider, as captured from the official
// release: the AI SDK User-Agent of the package serving the protocol, both
// session headers, the user message id as x-opencode-request (a fresh
// ascending msg_ id when none was derived), the git root commit as the
// project, the canonical parent session under both parent headers, and only
// the anthropic-beta flags the AI SDK derives from the body. The v1 client
// sends no x-session-affinity or X-Session-Id to this provider.
func setV1Headers(header http.Header, protocol wire.Protocol, body []byte, ids identity.RequestIDs) {
	header.Set("Accept", "*/*")
	header.Set("User-Agent", httpx.V1ModelUserAgent(v1ProviderUtils(protocol)))
	header.Set("x-opencode-client", "cli")
	header.Set("x-opencode-session", ids.Session)
	header.Set("x-opencode-session-id", ids.Session)
	message := ids.Message
	if message == "" {
		message = identity.AscendingID("msg", time.Now())
	}
	header.Set("x-opencode-request", message)
	header.Set("x-opencode-project", v1ProjectID(ids.Project))
	if ids.ParentSession != "" {
		parent := identity.CanonicalSessionID(ids.ParentSession)
		header.Set("x-opencode-parent-session-id", parent)
		header.Set("x-parent-session-id", parent)
	}
	if protocol == wire.Anthropic {
		if betas := v1AnthropicBetas(body); betas != "" {
			header.Set("anthropic-beta", betas)
		}
	}
}

// setV2Headers renders the OpenCode 2.x CLI header set the v2 gates
// fingerprint: the "opencode/<channel>/<version>/cli" User-Agent, every
// session header (x-opencode-session, x-opencode-session-id,
// x-session-affinity, X-Session-Id) set to the same descending-encoded id,
// and a bare 40-character hex x-opencode-project. The v2 gate rejects
// x-opencode-request and canonical prj_* project ids with 403 FreeTierError,
// so neither is sent. Anthropic requests carry a fixed anthropic-beta set.
func setV2Headers(header http.Header, protocol wire.Protocol, ids identity.RequestIDs) {
	header.Set("Accept", "application/json, text/event-stream")
	header.Set("x-opencode-client", "cli")
	rootSession := ids.Session
	if ids.ParentSession != "" {
		parent := identity.CanonicalSessionID(ids.ParentSession)
		rootSession = parent
		header.Set("x-opencode-parent-session-id", parent)
		header.Set("x-parent-session-id", parent)
	}
	header.Set("x-session-affinity", rootSession)
	header.Set("X-Session-Id", rootSession)
	header.Set("x-opencode-session", rootSession)
	header.Set("x-opencode-session-id", ids.Session)
	header.Set("User-Agent", httpx.V2UserAgent())
	header.Set("x-opencode-project", v2ProjectID(ids.Project))
	if protocol == wire.Anthropic {
		header.Set("anthropic-version", "2023-06-01")
		header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	}
}

// v1ProviderUtils names the provider-utils version of the AI SDK package the
// OpenCode 1.x client routes a protocol through, for the v1 User-Agent. System
// One has no v1 counterpart and reports the chat package.
func v1ProviderUtils(protocol wire.Protocol) string {
	switch protocol {
	case wire.Responses:
		return httpx.V1ResponsesSDK
	case wire.Anthropic:
		return httpx.V1AnthropicSDK
	default:
		return httpx.V1ChatSDK
	}
}

// gitCommitPattern matches the git root commit hash OpenCode 1.x uses as its
// project id.
var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// v1ProjectID renders the project header the way the OpenCode 1.x client
// does: the git root commit hash of the project, or "global" outside a git
// repository. A genuine value from a v1 client is kept verbatim; any other
// project is hashed into the commit shape (see v2ProjectID).
func v1ProjectID(project string) string {
	if project == "global" || gitCommitPattern.MatchString(project) {
		return project
	}
	return v2ProjectID(project)
}

// v1AnthropicBetaRequest holds the Messages body fields the AI SDK
// anthropic provider derives beta flags from. Fields whose JSON type varies
// stay raw so one unexpected value cannot hide the others.
type v1AnthropicBetaRequest struct {
	Model             string            `json:"model"`
	MCPServers        []json.RawMessage `json:"mcp_servers"`
	ContextManagement *struct {
		Edits []struct {
			Type string `json:"type"`
		} `json:"edits"`
	} `json:"context_management"`
	Container    json.RawMessage `json:"container"`
	OutputConfig struct {
		TaskBudget json.RawMessage `json:"task_budget"`
	} `json:"output_config"`
	Speed     json.RawMessage `json:"speed"`
	Fallbacks json.RawMessage `json:"fallbacks"`
	Thinking  struct {
		BlockBinding json.RawMessage `json:"block_binding"`
	} `json:"thinking"`
	Tools []struct {
		Type           string          `json:"type"`
		InputExamples  json.RawMessage `json:"input_examples"`
		AllowedCallers json.RawMessage `json:"allowed_callers"`
	} `json:"tools"`
}

// v1AnthropicToolBetas maps the server tool types of a Messages body to the
// beta flag the AI SDK anthropic provider sends with each.
var v1AnthropicToolBetas = map[string]string{
	"code_execution_20250522": "code-execution-2025-05-22",
	"code_execution_20250825": "code-execution-2025-08-25",
	"computer_20241022":       "computer-use-2024-10-22",
	"computer_20250124":       "computer-use-2025-01-24",
	"computer_20251124":       "computer-use-2025-11-24",
	"text_editor_20241022":    "computer-use-2024-10-22",
	"text_editor_20250124":    "computer-use-2025-01-24",
	"text_editor_20250429":    "computer-use-2025-01-24",
	"bash_20241022":           "computer-use-2024-10-22",
	"bash_20250124":           "computer-use-2025-01-24",
	"memory_20250818":         "context-management-2025-06-27",
	"web_fetch_20250910":      "web-fetch-2025-09-10",
	"web_fetch_20260209":      "code-execution-web-tools-2026-02-09",
	"web_search_20260209":     "code-execution-web-tools-2026-02-09",
	"advisor_20260301":        "advisor-tool-2026-03-01",
}

// v1AnthropicBetas returns the anthropic-beta value OpenCode 1.19.3 sends
// with a Messages body on the v1 lane, or "" when no flag applies. Its
// opencode provider sets no fixed betas: the AI SDK derives them from the
// request features (body features first, then tools, in the SDK's order), and
// per-tool eager_input_streaming has replaced the old
// fine-grained-tool-streaming flag.
func v1AnthropicBetas(body []byte) string {
	var request v1AnthropicBetaRequest
	// A field of an unexpected type fails alone; the rest still decode.
	_ = json.Unmarshal(body, &request)
	var betas []string
	add := func(beta string) {
		if !slices.Contains(betas, beta) {
			betas = append(betas, beta)
		}
	}
	if len(request.MCPServers) > 0 {
		add("mcp-client-2025-04-04")
	}
	if request.ContextManagement != nil {
		add("context-management-2025-06-27")
		for _, edit := range request.ContextManagement.Edits {
			if edit.Type == "compact_20260112" {
				add("compact-2026-01-12")
			}
		}
	}
	var container struct {
		Skills []json.RawMessage `json:"skills"`
	}
	if json.Unmarshal(request.Container, &container) == nil && len(container.Skills) > 0 {
		add("code-execution-2025-08-25")
		add("skills-2025-10-02")
		add("files-api-2025-04-14")
	}
	if isPresentJSON(request.OutputConfig.TaskBudget) {
		add("task-budgets-2026-03-13")
	}
	if string(request.Speed) == `"fast"` {
		add("fast-mode-2026-02-01")
	}
	var fallbacks []json.RawMessage
	if string(request.Fallbacks) == `"default"` {
		add("server-side-fallback-2026-07-01")
	} else if json.Unmarshal(request.Fallbacks, &fallbacks) == nil && len(fallbacks) > 0 {
		add("server-side-fallback-2026-06-01")
	}
	if isPresentJSON(request.Thinking.BlockBinding) {
		add("thinking-binding-controls-2026-08-01")
	}
	for _, tool := range request.Tools {
		if tool.Type == "" || tool.Type == "custom" {
			if anthropicSupportsStructuredOutput(request.Model) {
				add("structured-outputs-2025-11-13")
			}
			if isPresentJSON(tool.InputExamples) || isPresentJSON(tool.AllowedCallers) {
				add("advanced-tool-use-2025-11-20")
			}
		} else if beta := v1AnthropicToolBetas[tool.Type]; beta != "" {
			add(beta)
		}
	}
	return strings.Join(betas, ",")
}

func isPresentJSON(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// claude3OrOlderPattern matches the Claude model families older than Claude 4
// (Claude Instant, Claude 2 and every Claude 3.x model) that the AI SDK treats
// as lacking structured output.
var claude3OrOlderPattern = regexp.MustCompile(`claude-(?:instant(?:-|$)|v?2(?:$|[-.:])|3(?:$|[-.]))`)

// anthropicSupportsStructuredOutput mirrors the AI SDK anthropic provider's
// model capability table, checked in the same order: the dated Claude 4.0
// releases and Claude 3 and older lack structured output, every other Claude
// model has it, and non-Claude models do not.
func anthropicSupportsStructuredOutput(model string) bool {
	for _, supported := range []string{
		"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-fable-5", "claude-sonnet-5",
		"claude-sonnet-4-6", "claude-opus-4-6", "claude-sonnet-4-5", "claude-opus-4-5", "claude-haiku-4-5",
		"claude-opus-4-1",
	} {
		if strings.Contains(model, supported) {
			return true
		}
	}
	if strings.Contains(model, "claude-sonnet-4-") || strings.Contains(model, "claude-opus-4-") ||
		strings.Contains(model, "claude-3-haiku") || claude3OrOlderPattern.MatchString(model) {
		return false
	}
	return strings.Contains(model, "claude-")
}

// v2ProjectID renders the project header in the shape the OpenCode 2.x CLI
// sends to the v2 gates: a bare 40-character lowercase hex id, here the first
// 20 bytes of a domain-separated SHA-256 of the project. The value itself is
// not checked upstream, only its shape, but hashing keeps one project mapped
// to one stable upstream project id. v1ProjectID reuses it to fit a
// non-genuine project into the git commit shape.
func v2ProjectID(project string) string {
	if project == "global" || gitCommitPattern.MatchString(project) {
		return project
	}
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

// isNonRetryableClientResponse reports whether resp is a 4xx that retrying on
// another key of the same tier cannot fix: every 4xx except 401, 403 and 429,
// which can be specific to the key. A transport error is never non-retryable.
func isNonRetryableClientResponse(resp *http.Response, err error) bool {
	return err == nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests
}
