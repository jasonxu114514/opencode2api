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

type probeResult struct {
	success, attempted, effortRejected bool
	reason, channel                    string
	// proxy is the redacted exit that served the reported attempt, so a
	// per-IP 403/429 can be told apart from a broken proxy or a bad model.
	proxy  string
	effort models.EffortProbe
}

func (g *Gateway) resolvedEffort(model string, tier config.Tier, protocol wire.Protocol) string {
	effort := g.cfg.ForcedEffort(model)
	if effort != "auto" {
		return effort
	}
	// Only the free Zen lane is actively probed. Never transfer observations
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

func probePayload(model string, protocol wire.Protocol, effort string, legacy bool) ([]byte, error) {
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
	shaped, _ := prepareAnonymousBody(b, protocol, legacy)
	return shaped, nil
}

// Probe scans are bounded: one probe is a full upstream request, so sweeping
// every configured exit on every availability cycle would multiply probe
// traffic by the pool size. Two siblings per lane are enough to distinguish
// "this exit is out of quota" from "the model is gone".
const (
	maxProbeAnonymousExits = 2
	maxProbeKeys           = 2
)

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
		proxy              string
	}
	var lanes []lane
	if g.cfg.Anonymous {
		// Probe a couple of sibling anonymous exits, not just the
		// affinity-pinned first one: the anonymous lane is rate-limited per
		// exit IP, so one quota-exhausted exit must not fail the whole model
		// while others serve it fine. The scan is capped because every probe
		// is a real upstream request and an unbounded sweep would multiply
		// probe traffic by the number of configured exits.
		cursor := g.anonymous.CursorFor(ids.Session)
		exits := 0
		for attempt := 0; attempt < g.anonymous.Len() && exits < maxProbeAnonymousExits; attempt++ {
			node := cursor.Next()
			if node == nil {
				break
			}
			exits++
			// Prefer the legacy /zen/v1 lane before falling back to /inference.
			for _, baseURL := range g.anonymousUpstreams() {
				lanes = append(lanes, lane{anonymousZenKey, "anonymous", baseURL, node.proxy.client, config.RedactURL(node.proxy.name)})
			}
		}
	}
	// Likewise, one bad key must not decide the verdict while sibling keys
	// in the same tier are healthy. Bounded for the same traffic reason.
	seen := make(map[*upstreamNode]bool, g.zenNodes.Len())
	zcursor := g.zenNodes.CursorFor(ids.Session)
	keys := 0
	for i := 0; i < g.zenNodes.Len() && keys < maxProbeKeys; i++ {
		node := zcursor.Next()
		if node == nil || seen[node] {
			break
		}
		seen[node] = true
		if proxy := g.zenNodes.Proxy(node); proxy != nil {
			keys++
			lanes = append(lanes, lane{node.key, "zen", g.cfg.Upstream.Zen, proxy.client, config.RedactURL(proxy.name)})
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
		result.proxy = lane.proxy
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

func (g *Gateway) probeAttempt(parent context.Context, client *http.Client, baseURL, key, model string, protocol wire.Protocol, ids identity.RequestIDs, effort string) probeResult {
	b, err := probePayload(model, protocol, effort, isLegacyZenUpstream(baseURL))
	if err != nil {
		return probeResult{reason: "request_conversion_error"}
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	ids.Request = identity.RandomID("req", 16)
	req, err := newUpstreamRequest(ctx, baseURL, protocol, b, ids, key)
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
