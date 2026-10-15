package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	"opencode2api/internal/telemetry"
)

// continuationInstruction is the empirically verified user message appended
// after the delivered partial; the free tier has no assistant prefill, so a
// bare trailing assistant message regenerates from scratch instead of
// continuing.
const continuationInstruction = "Continue exactly where you stopped. Do not repeat anything already written."

const reasoningContinuationInstruction = "You have already completed the following detailed reasoning process:\n\n<thinking>\n%s\n</thinking>\n\nBased on your reasoning above, do not repeat or re-evaluate the thinking. Output your final answer directly now."

const maxContinuationPartialBytes = 512 << 10

var errContinuationTimeout = errors.New("stream continuation fetch timed out")

type gatewayStreamResumer struct {
	g                   *Gateway
	clientCtx           context.Context
	streamState         *anonymousStreamState
	continuationTimeout time.Duration
	route               models.Route
	ids                 identity.RequestIDs
	zenBody             []byte
	meta                *telemetry.RequestMeta
	attemptOffset       int
}

// ResumeSegment builds the continuation body from the delivered partial,
// cools down the exit that just broke so the pool cursor rotates onto a
// different one, and fetches the next segment on a client-bound budget.
func (r *gatewayStreamResumer) ResumeSegment(partial string, reasoning string) (io.Reader, error) {
	if partial == "" && reasoning == "" {
		r.recordResumeFailure("empty_partial")
		return nil, errors.New("stream continuation requires delivered text or reasoning")
	}
	if len(partial) > maxContinuationPartialBytes {
		r.recordResumeFailure("partial_too_large")
		return nil, fmt.Errorf("stream continuation partial of %d bytes exceeds the %d byte limit", len(partial), maxContinuationPartialBytes)
	}
	if len(reasoning) > maxContinuationPartialBytes {
		reasoning = reasoning[len(reasoning)-maxContinuationPartialBytes:]
	}
	if r.clientCtx == nil {
		r.recordResumeFailure("missing_client_context")
		return nil, errors.New("stream continuation requires a client context")
	}
	if err := r.clientCtx.Err(); err != nil {
		r.recordResumeSkipped("client_canceled", len(partial), len(reasoning), false)
		return nil, err
	}
	if r.continuationTimeout <= 0 {
		r.recordResumeSkipped("budget_disabled", len(partial), len(reasoning), false)
		return nil, errors.New("stream continuation budget is disabled")
	}
	requestBody, err := buildContinuationBody(r.zenBody, partial, reasoning)
	if err != nil {
		r.recordResumeFailure("invalid_continuation_body")
		return nil, err
	}
	continuationCtx, cancel := context.WithCancelCause(r.clientCtx)
	continuationCtx = context.WithValue(continuationCtx, anonymousStreamStateKey{}, r.streamState)
	var timeoutMu sync.Mutex
	timeoutCommitted := false
	timer := time.AfterFunc(r.continuationTimeout, func() {
		timeoutMu.Lock()
		defer timeoutMu.Unlock()
		if !timeoutCommitted {
			cancel(errContinuationTimeout)
		}
	})
	keepContext := false
	defer func() {
		timer.Stop()
		if !keepContext {
			cancel(nil)
		}
	}()
	r.g.markCurrentStreamExitFailed(continuationCtx)
	bodies := map[config.Tier][]byte{config.TierZen: requestBody}
	if r.meta != nil {
		r.meta.ResumeAttempts++
		if partial != "" {
			r.meta.ResumeKind = "text"
		} else {
			r.meta.ResumeKind = "reasoning"
		}
	}
	if r.g.logger != nil {
		kind := ""
		if r.meta != nil {
			kind = r.meta.ResumeKind
		}
		r.g.logger.Info("stream continuation attempt dispatched", "component", "upstream", "event", "stream_continuation_attempted", "request_id", r.ids.Request, "model", r.route.ID, "partial_bytes", len(partial), "reasoning_bytes", len(reasoning), "resume_kind", kind, "timeout_ms", r.continuationTimeout.Milliseconds())
	}
	resp, err, attempts := r.g.doAnonymousUpstream(continuationCtx, r.route, bodies, r.ids, r.attemptOffset)
	r.attemptOffset += attempts
	timeoutMu.Lock()
	cause := context.Cause(continuationCtx)
	if cause == nil {
		timeoutCommitted = true
		timer.Stop()
	}
	timeoutMu.Unlock()
	if cause != nil {
		if resp != nil && resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		resp = nil
		err = cause
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		reason := "upstream_error"
		if errors.Is(err, errContinuationTimeout) {
			reason = "continuation_timeout"
		} else if r.clientCtx.Err() != nil {
			reason = "client_canceled"
		}
		r.recordResumeFailure(reason)
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		status := resp.StatusCode
		if resp.Body != nil {
			httpx.DrainAndClose(resp.Body)
		}
		r.recordResumeFailure("upstream_http_error")
		return nil, fmt.Errorf("stream continuation upstream returned status %d", status)
	}
	if resp.Body == nil {
		r.recordResumeFailure("upstream_empty_body")
		return nil, errors.New("stream continuation upstream returned no body")
	}
	continued := 0
	if r.meta != nil {
		r.meta.Continued++
		r.meta.ResumeSuccesses++
		r.meta.Legs = append(r.meta.Legs, "cont:resume")
		continued = r.meta.Continued
	}
	keepContext = true
	if r.g.logger != nil {
		kind := ""
		if r.meta != nil {
			kind = r.meta.ResumeKind
		}
		r.g.logger.Info("stream continued on a new exit", "component", "upstream", "event", "stream_continued", "request_id", r.ids.Request, "model", r.route.ID, "partial_bytes", len(partial), "reasoning_bytes", len(reasoning), "resume_kind", kind, "resume_attempt", continued)
	}
	return &continuationBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

type continuationBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (body *continuationBody) Close() error {
	body.cancel(nil)
	return body.ReadCloser.Close()
}

func (r *gatewayStreamResumer) ResumeEligible(visibleBytes, reasoningBytes int, toolSeen bool) {
	if r.meta != nil {
		r.meta.ResumeVisibleBytes = visibleBytes
		r.meta.ResumeReasoningBytes = reasoningBytes
		r.meta.ResumeToolSeen = toolSeen
		if visibleBytes == 0 && reasoningBytes > 0 {
			r.meta.ResumeKind = "reasoning"
		} else {
			r.meta.ResumeKind = "text"
		}
	}
}

func (r *gatewayStreamResumer) ResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	if r.meta != nil {
		if r.meta.ResumeSkipped == "" {
			r.meta.ResumeSkipped = reason
		}
		r.meta.ResumeVisibleBytes = visibleBytes
		r.meta.ResumeReasoningBytes = reasoningBytes
		r.meta.ResumeToolSeen = toolSeen
	}
	if r.g != nil && r.g.logger != nil {
		r.g.logger.Debug("stream continuation skipped", "component", "upstream", "event", "stream_continuation_skipped", "request_id", r.ids.Request, "model", r.route.ID, "reason", reason, "visible_bytes", visibleBytes, "reasoning_bytes", reasoningBytes, "tool_seen", toolSeen)
	}
}

func (r *gatewayStreamResumer) recordResumeSkipped(reason string, visibleBytes, reasoningBytes int, toolSeen bool) {
	r.ResumeSkipped(reason, visibleBytes, reasoningBytes, toolSeen)
}

func (r *gatewayStreamResumer) recordResumeFailure(reason string) {
	if r.meta != nil {
		r.meta.ResumeFailures++
	}
	if r.g != nil && r.g.logger != nil {
		r.g.logger.Warn("stream continuation failed", "component", "upstream", "event", "stream_continuation_failed", "request_id", r.ids.Request, "model", r.route.ID, "reason", reason)
	}
}

// buildContinuationBody appends the delivered partial plus the continuation
// instruction to the original Zen body. A trailing assistant message is
// merged in place so the request never carries two adjacent assistant turns;
// every other field (tools, stream options, effort, ...) is preserved. The
// anonymous free-tier shape is NOT re-asserted here: doAnonymousUpstream
// already runs prepareAnonymousBody on every body it sends, so shaping here
// as well was a redundant second pass (review: "prepareAnonymousBody called
// twice").
func buildContinuationBody(zenBody []byte, partial string, reasoning string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(zenBody, &payload); err != nil {
		return nil, fmt.Errorf("parse continuation base body: %w", err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("continuation base body has no messages")
	}
	if partial != "" {
		if last, ok := messages[len(messages)-1].(map[string]any); ok && last["role"] == "assistant" {
			// Multimodal turns carry content-part arrays; flatten the text
			// parts so the partial can still merge (review: array content
			// broke construction).
			last["content"] = flattenMessageContent(last["content"]) + partial
		} else {
			messages = append(messages, map[string]any{"role": "assistant", "content": partial})
		}
		messages = append(messages, map[string]any{"role": "user", "content": continuationInstruction})
	} else if reasoning != "" {
		instruction := "\n\n" + fmt.Sprintf(reasoningContinuationInstruction, reasoning)
		if last, ok := messages[len(messages)-1].(map[string]any); ok && last["role"] == "user" {
			if content, ok := last["content"].(string); ok {
				last["content"] = content + instruction
			} else {
				messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf(reasoningContinuationInstruction, reasoning)})
			}
		} else {
			messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf(reasoningContinuationInstruction, reasoning)})
		}
	}
	payload["messages"] = messages
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("continuation body contains unsupported JSON values")
	}
	return encoded, nil
}

// flattenMessageContent renders a Chat message content value (string or
// content-part array) as plain text.
func flattenMessageContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var out strings.Builder
		for _, part := range value {
			if entry, ok := part.(map[string]any); ok {
				if text, ok := entry["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	default:
		return ""
	}
}

// streamResumeLimit returns the number of resumes allowed after the first
// break; values below 1 mean one resume, matching the shipped default.
func (g *Gateway) streamResumeLimit() int {
	limit := g.cfg.Performance.StreamContinuationMax
	if limit < 1 {
		limit = 1
	}
	return limit
}

func attemptOffsetFromMeta(meta *telemetry.RequestMeta) int {
	if meta == nil {
		return 0
	}
	return meta.Attempts
}
