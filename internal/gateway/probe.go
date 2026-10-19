package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

// probeResult is the outcome of a free-model probe: whether it succeeded and
// whether any request was sent at all, whether the upstream rejected the
// requested reasoning effort, a reason code, the channel ("anonymous" or
// "zen") that produced it, and the effort observation to store.
type probeResult struct {
	success, attempted, effortRejected bool
	reason, channel                    string
	effort                             models.EffortProbe
}

// resolvedEffort returns the reasoning effort to force on a request for model:
// the configured effort unless it is "auto", otherwise the level a recent
// availability probe confirmed for this protocol on the free Zen tier, or ""
// to inject none.
func (g *Gateway) resolvedEffort(model string, tier config.Tier, protocol wire.Protocol) string {
	effort := g.cfg.ForcedEffort(model)
	if effort != "auto" {
		return effort
	}
	// Only the free Zen tier is actively probed. Never transfer observations
	// to a different protocol/tier, or guess a level for an unprobed model.
	if tier != config.TierZen || protocol == wire.SystemOne || g.availability == nil || !g.catalog.IsFreeModel(model) {
		return ""
	}
	item := g.availability.Get(model)
	if !g.catalog.MetadataForTier(model, tier).Reasoning || item.AutoEffort.Effort == "none" {
		return "" // Do not inject reasoning into a model that only accepted the baseline.
	}
	if item.Disabled || item.AutoEffort.Protocol != string(protocol) || time.Since(item.EffortCheckedAt) > 2*models.FreeModelCheckInterval {
		return ""
	}
	return item.AutoEffort.Effort
}

// probePayload builds the probe body for model: a System One arithmetic
// question, or a minimal streaming "Reply with OK." Chat prompt converted to
// protocol, with effort forced when non-empty and the result shaped for the
// free tier by prepareAnonymousBody. v1 reports whether the probe targets the
// v1 lane (models.IsV1Upstream), which selects the single "read" tool shaping
// instead of the v2 toolset.
func probePayload(model string, protocol wire.Protocol, effort string, v1 bool) ([]byte, error) {
	if protocol == wire.SystemOne {
		// TypeSafe's native boolean type is "noul", not JSON Schema boolean.
		return json.Marshal(map[string]any{"model": model, "state": "1 + 1 = 2", "questions": map[string]any{
			"arithmetic": map[string]any{"type": "noul", "instructions": "Is 1 + 1 equal to 2?"},
		}})
	}
	payload := map[string]any{"model": model, "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Reply with OK."}}}
	converted, err := wire.ConvertRequest(wire.Chat, protocol, payload)
	if err != nil {
		return nil, err
	}
	if effort != "" {
		wire.ForcedEffort(protocol, converted, effort)
	}
	b, err := json.Marshal(converted)
	if err != nil {
		return nil, err
	}
	shaped, _ := prepareAnonymousBody(b, protocol, v1)
	return shaped, nil
}

// probeFreeModel checks whether a free model currently answers on the Zen
// tier. It tries one anonymous proxy against each anonymous upstream (in
// anonymousUpstreams order) and then one Zen key, and stops at the first lane
// that succeeds. For a reasoning model whose effort is "auto", a successful
// baseline is followed by trials from "max" downward to find the highest
// effort it accepts on this protocol. The reason is model_unavailable only
// when every lane reported it; any other failure keeps the result
// inconclusive.
func (g *Gateway) probeFreeModel(ctx context.Context, model string) probeResult {
	if !g.catalog.IsFreeModel(model) {
		return probeResult{reason: "not_free"}
	}
	d := g.catalog.Diagnostic(model, wire.Chat, g.zenNodes.Len() > 0, false, g.cfg.Anonymous)
	protocol := d.NativeProtocols[config.TierZen]
	if !d.AvailableZen || !g.catalog.Supported(model) {
		return probeResult{reason: "unsupported_probe"}
	}
	ids := identity.RequestIDs{Session: identity.CanonicalSessionID("availability:" + model), Project: "opencode2api:default-project"}
	type lane struct {
		key, name, baseURL string
		client             *http.Client
	}
	var lanes []lane
	if g.cfg.Anonymous {
		cursor := g.anonymous.CursorFor(ids.Session)
		if node := cursor.Next(); node != nil {
			// Prefer the v1 lane (/zen) before falling back to the v2
			// /inference origin.
			for _, baseURL := range g.anonymousUpstreams() {
				lanes = append(lanes, lane{anonymousZenKey, "anonymous", baseURL, g.inferenceClient(node.proxy, baseURL, model)})
			}
		}
	}
	cursor := g.zenNodes.CursorFor(ids.Session)
	if node := cursor.Next(); node != nil {
		if proxy := g.zenNodes.Proxy(node); proxy != nil {
			lanes = append(lanes, lane{node.key, "zen", g.cfg.Upstream.Zen, g.inferenceClient(proxy, g.cfg.Upstream.Zen, model)})
		}
	}
	result := probeResult{reason: "no_probe_channel"}
	inconclusive := ""
	for _, lane := range lanes {
		if ctx.Err() != nil {
			return probeResult{reason: "canceled"}
		}
		run := func(effort string) probeResult {
			return g.probeAttempt(ctx, lane.client, lane.baseURL, lane.key, model, protocol, ids, effort)
		}
		result = run("")
		result.channel = lane.name
		if result.success {
			if g.cfg.ForcedEffort(model) == "auto" && protocol != wire.SystemOne {
				if !g.catalog.MetadataForTier(model, config.TierZen).Reasoning {
					result.effort = models.EffortProbe{Effort: "none", Protocol: string(protocol)}
				} else {
					for _, effort := range []string{"max", "xhigh", "high", "medium", "low", "minimal", "none"} {
						if effort == "none" {
							// Baseline already succeeded without an effort field.
							result.effort = models.EffortProbe{Effort: "none", Protocol: string(protocol)}
							break
						}
						trial := run(effort)
						if trial.success {
							result.effort = models.EffortProbe{Effort: effort, Protocol: string(protocol)}
							break
						}
						// A rate limit or timeout gives no evidence that a lower
						// reasoning level is required. Keep previous observations.
						if !trial.effortRejected {
							break
						}
						// Once higher levels were explicitly rejected, do not
						// retain an older ceiling if the next trial is inconclusive.
						result.effort = models.EffortProbe{Reset: true}
					}
				}
			}
			return result
		}
		if result.reason != "model_unavailable" {
			inconclusive = result.reason
		}
	}
	// One lane's auth/transient failure cannot establish global unavailability.
	if inconclusive != "" {
		result.reason = inconclusive
	}
	return result
}

// probeAttempt sends one probe request through client with a 60-second budget
// and classifies the result. Unlike live traffic it bypasses
// doInferenceAttempt and records nothing against keys or proxies. Success
// requires a 2xx whose SSE stream collapses cleanly or whose JSON document has
// the protocol's result field; a failure carries a reason code (http_<status>,
// model_unavailable, probe_timeout, ...) and whether the upstream rejected the
// reasoning effort.
func (g *Gateway) probeAttempt(parent context.Context, client *http.Client, baseURL, key, model string, protocol wire.Protocol, ids identity.RequestIDs, effort string) probeResult {
	b, err := probePayload(model, protocol, effort, models.IsV1Upstream(baseURL))
	if err != nil {
		return probeResult{reason: "request_conversion_error"}
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	ids.Request = identity.RandomID("req", 16)
	req, err := upstreamRequest(ctx, baseURL, protocol, b, ids, key)
	if err != nil {
		return probeResult{reason: "request_creation_error"}
	}
	result := probeResult{attempted: true}
	resp, err := client.Do(req)
	if err != nil {
		result.reason = "transport_error"
		if ctx.Err() != nil {
			result.reason = "probe_timeout"
		}
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result.reason = fmt.Sprintf("http_%d", resp.StatusCode)
		if readErr == nil {
			result.effortRejected = isEffortRejection(resp.StatusCode, string(body))
			if isModelUnavailable(resp.StatusCode, string(body)) {
				result.reason = "model_unavailable"
			}
		}
		return result
	}
	reader := io.LimitReader(resp.Body, 1<<20)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") && protocol != wire.SystemOne {
		_, err = wire.CollapseStream(reader, protocol, model)
	} else {
		var document map[string]any
		err = json.NewDecoder(reader).Decode(&document)
		if err == nil && (document == nil || document["error"] != nil) {
			err = fmt.Errorf("upstream error envelope: %v", document["error"])
		}
		if err == nil {
			switch protocol {
			case wire.SystemOne:
				answer := jsonutil.MapAt(document, "answers", "arithmetic")
				probability, ok := answer["noul"].(float64)
				if !ok || probability < 0 || probability > 1 || answer["type"] != "noul" {
					err = fmt.Errorf("invalid System One answer")
				}
			default:
				field := "choices"
				if protocol == wire.Anthropic {
					field = "content"
				} else if protocol == wire.Responses {
					field = "output"
				}
				if _, ok := document[field].([]any); !ok {
					err = fmt.Errorf("invalid response schema")
				}
			}
		}
	}
	result.success = err == nil && ctx.Err() == nil
	result.reason = "probe_succeeded"
	if !result.success {
		result.reason = "invalid_or_incomplete_response"
		if err != nil {
			result.effortRejected = isEffortRejection(400, err.Error())
			if isModelUnavailable(400, err.Error()) {
				result.reason = "model_unavailable"
			}
		}
		if ctx.Err() != nil {
			result.reason = "probe_timeout"
		}
	}
	return result
}

// isModelUnavailable reports whether a 400, 404, 410 or 422 error body says
// the model itself is unknown, retired or no longer available, as opposed to
// a transient or credential failure.
func isModelUnavailable(status int, body string) bool {
	if status != 400 && status != 404 && status != 410 && status != 422 {
		return false
	}
	text := strings.ToLower(body)
	for _, marker := range []string{"model_not_found", "model_not_available", "model_retired", "model_deprecated", "unknown model", "model does not exist", "model has been retired", "model is no longer available"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// isEffortRejection reports whether a 400 or 422 error body rejects the
// requested reasoning level: it must mention effort, thinking or reasoning
// together with a validation marker such as "invalid" or "unsupported".
func isEffortRejection(status int, body string) bool {
	if status != 400 && status != 422 {
		return false
	}
	text := strings.ToLower(body)
	if !strings.Contains(text, "effort") && !strings.Contains(text, "thinking") && !strings.Contains(text, "reasoning") {
		return false
	}
	for _, marker := range []string{"invalid", "unsupported", "not support", "not allowed", "must be", "allowed values"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
