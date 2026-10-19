package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"opencode2api/internal/rotation"
	"opencode2api/internal/telemetry"
)

// syncRotation tells the rotation manager which catalog models the Go tier can
// serve right now: every model with a Go route except System One models, and
// none while no Go key is configured.
func (g *Gateway) syncRotation() {
	available := map[string]bool{}
	for _, model := range g.catalog.List() {
		if route, err := g.catalog.RouteForTier(model, config.TierGo, false, g.goNodes.Len() > 0); err == nil && route.Protocol != wire.SystemOne {
			available[model] = true
		}
	}
	g.rotation.Sync(available)
}

// RotationSnapshot resyncs the rotation groups with the current catalog and
// returns their state.
func (m *RuntimeController) RotationSnapshot() rotation.Snapshot {
	r := m.current.Load()
	r.gateway.syncRotation()
	return m.rotation.Snapshot()
}

// SelectRotation manually switches rotation group id to model, clearing any
// pause on it, after resyncing the groups with the current catalog. model
// must be one of the group's currently available models.
func (m *RuntimeController) SelectRotation(id, model string) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	m.current.Load().gateway.syncRotation()
	return m.rotation.Manual(id, model)
}

// quotaError reports whether a failed rotation attempt hit a quota or rate
// limit, which pauses the model for its Retry-After instead of spending the
// request's ordinary-failure budget. Any 429 counts; otherwise only explicit
// quota markers in the body do, since ordinary provider errors must keep
// their budget.
func quotaError(status int, body string) bool {
	if status == 429 {
		return true
	}
	body = strings.ToLower(body)
	for _, marker := range []string{"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted", "rate_limit", "rate limit", "rate-limit", "usage_limit", "usage limit", "insufficient balance", "credit balance is too low", "额度不足", "配额已用尽"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// groupAttempt is the outcome of one rotation attempt. quota marks a quota or
// rate-limit failure, committed means the reply already reached the client,
// pause is how long a quota failure pauses the model, and reason labels the
// attempt record.
type groupAttempt struct {
	err              error
	quota, committed bool
	pause            time.Duration
	reason           string
}

// handleRotation serves a request addressed to a rotation group alias on the
// Go tier. It walks the group's usable models from the current one, giving
// each up to three single-call attempts. A quota or rate-limit failure pauses
// the model for its Retry-After (60 seconds without one) and moves on to the
// next model; a model that fails a third time for another reason is paused
// for 15 seconds. The walk ends on success, once a reply has been committed
// downstream, once three models have failed for ordinary reasons, when no
// usable model is left, or when the request budget runs out. Unless the client
// went away, a request left without a reply then gets a 503, or a 504 when
// the budget ran out.
func (g *Gateway) handleRotation(w http.ResponseWriter, r *http.Request, external wire.Protocol, payload map[string]any, id, alias string) {
	g.syncRotation()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	ids := identity.DeriveRequestIDs(r, payload, callerScope(r.Context()))
	meta := telemetry.MetaFromRequest(r)
	if meta != nil {
		meta.Request = ids.Request
		meta.Session = ids.Session
		meta.ModelAlias = alias
	}
	visited := map[string]bool{}
	ordinaryModels, total := 0, 0
	for ctx.Err() == nil {
		token, ok := g.rotation.Select(id, visited)
		if !ok {
			break
		}
		visited[token.Model] = true
		route, err := g.catalog.RouteForTier(token.Model, config.TierGo, false, g.goNodes.Len() > 0)
		if err != nil {
			continue
		}
		counted := false
		for n := 1; n <= 3 && ctx.Err() == nil; n++ {
			total++
			if meta != nil {
				meta.Model = token.Model
				meta.Tier = "go"
				meta.Protocol = route.Protocol
				meta.Attempts = total
				meta.Usage = wire.Usage{}
				meta.UsageReported = false
			}
			result := g.rotationAttempt(ctx, w, r, external, payload, alias, route, ids, total)
			outcome := result.reason
			if result.err == nil {
				outcome = "success"
			}
			var clientWrite *wire.ClientWriteError
			canceled := r.Context().Err() != nil || errors.As(result.err, &clientWrite)
			if canceled {
				outcome = "client_canceled"
				if meta != nil {
					meta.Outcome = outcome
				}
			}
			g.rotation.Record(rotation.Attempt{Request: ids.Request, Alias: alias, Model: token.Model, Number: total, Outcome: outcome})
			if state := g.rotation.Snapshot(); state.PersistenceError != "" {
				g.logger.Error("rotation state persistence failed", "component", "rotation", "event", "rotation_persistence_failed")
			}
			g.logger.Info("model rotation attempt", "component", "rotation", "event", "rotation_attempt", "request_id", ids.Request, "alias", alias, "model", token.Model, "attempt", total, "reason", outcome)
			if canceled {
				return
			}
			if result.err == nil {
				return
			}
			if result.committed {
				if meta != nil {
					meta.Outcome = "stream_error"
				}
				pause := 15 * time.Second
				if result.quota {
					pause = result.pause
				}
				g.rotation.Fail(token, pause, result.reason)
				return
			}
			if ctx.Err() != nil {
				g.rotation.Fail(token, 15*time.Second, "request_timeout")
				break
			}
			if result.quota {
				g.rotation.Fail(token, result.pause, result.reason)
				break
			}
			if !counted {
				ordinaryModels++
				counted = true
			}
			if n == 3 {
				g.rotation.Fail(token, 15*time.Second, result.reason)
			}
		}
		if ordinaryModels >= 3 {
			break
		}
	}
	if r.Context().Err() != nil {
		return
	}
	status := http.StatusServiceUnavailable
	message := "该分组暂无可用模型（已暂停、下架、额度不足或重试次数已用尽），请稍后重试"
	if ctx.Err() != nil {
		status = http.StatusGatewayTimeout
		message = "分组请求整体超时"
	}
	wire.WriteError(w, external, status, message, "rotation_unavailable", ids.Request)
}

// rotationAttempt makes one upstream call for a rotation group model with the
// attempt-th key the session's Go cursor yields. Nothing reaches the client
// unless the first useful output (for a non-streaming reply, the whole body)
// arrives within the rotation's first_output_seconds, and the reply names the
// group alias as its model.
func (g *Gateway) rotationAttempt(parent context.Context, w http.ResponseWriter, original *http.Request, external wire.Protocol, payload map[string]any, alias string, route models.Route, ids identity.RequestIDs, attempt int) groupAttempt {
	result := groupAttempt{reason: "upstream_error", pause: 60 * time.Second}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	seconds := g.rotation.Snapshot().FirstOutputSeconds
	timer := time.AfterFunc(time.Duration(seconds)*time.Second, cancel)
	defer timer.Stop()
	copyPayload := make(map[string]any, len(payload))
	for k, v := range payload {
		copyPayload[k] = v
	}
	copyPayload["model"] = route.ID
	bodies, err := g.prepareRouteBodies(external, route, copyPayload)
	if err != nil {
		result.err = err
		result.reason = "request_conversion_error"
		return result
	}
	body, toolAlias, shaped := g.shapeKeyBody(bodies[config.TierGo], route, config.TierGo)
	setRequestShape(parent, shaped, toolAlias)
	cursor := g.goNodes.CursorFor(ids.Session)
	var node *upstreamNode
	// Each group attempt is exactly one HTTP call. Key selection may change,
	// but there is no nested retry, stale-reasoning replay, or model-wide key ban.
	for n := 0; n < attempt; n++ {
		node = cursor.Next()
	}
	if node == nil {
		result.err = errors.New("no Go key")
		return result
	}
	proxy := g.goNodes.Proxy(node)
	if proxy == nil {
		result.err = errors.New("no proxy")
		return result
	}
	req, err := upstreamRequest(ctx, g.cfg.Upstream.Go, route.Protocol, body, ids, node.key)
	if err != nil {
		result.err = err
		return result
	}
	// A reused connection must not cause net/http to implicitly replay POSTs.
	req.GetBody = nil
	client := *proxy.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	setRequestCredential(parent, config.TierGo, config.KeyDisplayID(node.key), "key", false, proxy)
	started := time.Now()
	if proxy.pool {
		req.Close = true
	}
	resp, err := doInferenceAttempt(&client, req, 0, g.attemptTimeout)
	if err != nil && ctx.Err() != nil && parent.Err() == nil && !isProxyFailure(err) {
		// The rotation-specific first-output deadline is not evidence that the
		// proxy route failed. It still counts as a failed key attempt.
		err = errFirstEventTimeout
	}
	g.recordUpstreamAttempt(parent, route, ids, attempt, config.KeyDisplayID(node.key), "key", false, proxy, resp, err, time.Since(started))
	if resp != nil {
		defer resp.Body.Close()
	}
	if parent.Err() == nil {
		g.observeKeyResult(parent, g.goNodes, node, proxy, resp, err)
	}
	if err != nil {
		result.err = err
		if errors.Is(err, errFirstEventTimeout) || ctx.Err() != nil {
			result.reason = "first_output_timeout"
		}
		return result
	}
	retryAfter := resp.Header.Get("Retry-After")
	if pause := parseRetryAfter(retryAfter); pause > 0 {
		result.pause = pause
	} else if strings.TrimSpace(retryAfter) == "0" {
		result.pause = 0
	} else if _, err := http.ParseTime(retryAfter); err == nil {
		result.pause = 0
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		result.err = fmt.Errorf("upstream HTTP %d", resp.StatusCode)
		result.quota = quotaError(resp.StatusCode, string(b))
		result.reason = fmt.Sprintf("http_%d", resp.StatusCode)
		if result.quota {
			result.reason = "quota_or_rate_limit"
		}
		return result
	}
	stream := jsonutil.BoolAt(payload, "stream")
	ready := func() error {
		if !timer.Stop() || ctx.Err() != nil {
			return context.DeadlineExceeded
		}
		w.Header().Set("X-Resolved-Model", route.ID)
		w.Header().Set("x-request-id", ids.Request)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		return nil
	}
	meta := telemetry.MetaFromContext(parent)
	if stream {
		if meta != nil {
			meta.Stream = true
		}
		if g.monitor != nil {
			g.monitor.BeginStream()
			defer g.monitor.EndStream()
		}
		streamBody := wire.RenameStreamReader(resp.Body, route.Protocol, v1ReadTool, toolAlias)
		usage, reported, committed, err := wire.RotationStream(original.Context(), w, streamBody, route.Protocol, external, alias, ready)
		if meta != nil {
			meta.Usage = usage
			meta.UsageReported = reported
		}
		result.err = err
		result.committed = committed
		result.reason = "stream_error"
		var failure *wire.StreamFailure
		if errors.As(err, &failure) {
			result.quota = quotaError(0, failure.Type+" "+failure.Message)
		}
		if result.quota {
			result.reason = "quota_or_rate_limit"
		} else if !committed && ctx.Err() != nil {
			result.reason = "first_output_timeout"
		}
		return result
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err == nil && len(b) > 64<<20 {
		err = errors.New("response exceeds size limit")
	}
	if err != nil {
		result.err = err
		result.reason = "response_read_error"
		return result
	}
	if !timer.Stop() || ctx.Err() != nil {
		result.err = context.DeadlineExceeded
		result.reason = "first_output_timeout"
		return result
	}
	if shaped {
		b, err = wire.CollapseStream(bytes.NewReader(b), route.Protocol, route.ID)
	}
	if err == nil {
		b = wire.RenameResponseTool(b, route.Protocol, v1ReadTool, toolAlias)
	}
	var document map[string]any
	if err == nil {
		err = json.Unmarshal(b, &document)
	}
	if err == nil && document == nil {
		err = errors.New("empty upstream response")
	}
	if err == nil && document["error"] != nil {
		result.quota = quotaError(0, string(b))
		err = errors.New("upstream error envelope")
	}
	if err == nil {
		valid := false
		switch route.Protocol {
		case wire.Chat:
			_, valid = document["choices"].([]any)
		case wire.Anthropic:
			_, valid = document["content"].([]any)
		case wire.Responses:
			_, valid = document["output"].([]any)
		}
		if !valid {
			err = errors.New("invalid response schema")
		}
	}
	if err == nil && external != route.Protocol {
		b, err = wire.ConvertResponse(route.Protocol, external, b)
	}
	if err != nil {
		result.err = err
		result.reason = "response_conversion_error"
		if result.quota {
			result.reason = "quota_or_rate_limit"
		}
		return result
	}
	if meta != nil {
		meta.Usage, meta.UsageReported = wire.ResponseUsage(external, b)
	}
	document = nil
	if err = json.Unmarshal(b, &document); err != nil {
		result.err = err
		result.reason = "response_conversion_error"
		return result
	}
	document["model"] = alias
	b, err = json.Marshal(document)
	if err != nil {
		result.err = err
		return result
	}
	w.Header().Set("X-Resolved-Model", route.ID)
	w.Header().Set("x-request-id", ids.Request)
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(b)
	if err != nil {
		result.err = &wire.ClientWriteError{Err: err}
	}
	result.committed = true
	return result
}
