package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// continuationInstruction is the empirically verified user message appended
// after the delivered partial; the free tier has no assistant prefill, so a
// bare trailing assistant message regenerates from scratch instead of
// continuing.
const continuationInstruction = "Continue exactly where you stopped. Do not repeat anything already written."

const maxContinuationPartialBytes = 512 << 10

type gatewayStreamResumer struct {
	g             *Gateway
	requestCtx    context.Context
	route         models.Route
	ids           identity.RequestIDs
	zenBody       []byte
	meta          *telemetry.RequestMeta
	attemptOffset int
}

// ResumeSegment builds the continuation body from the delivered partial,
// cools down the exit that just broke so the pool cursor rotates onto a
// different one, and fetches the next segment on the same request budget.
func (r *gatewayStreamResumer) ResumeSegment(partial string) (io.Reader, error) {
	if partial == "" {
		return nil, errors.New("stream continuation requires delivered text")
	}
	if len(partial) > maxContinuationPartialBytes {
		return nil, fmt.Errorf("stream continuation partial of %d bytes exceeds the %d byte limit", len(partial), maxContinuationPartialBytes)
	}
	continuationBody, err := buildContinuationBody(r.zenBody, partial)
	if err != nil {
		return nil, err
	}
	r.g.markCurrentStreamExitFailed(r.requestCtx)
	bodies := map[config.Tier][]byte{config.TierZen: continuationBody}
	resp, err, attempts := r.g.doAnonymousUpstream(r.requestCtx, r.route, bodies, r.ids, r.attemptOffset)
	r.attemptOffset += attempts
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		status := resp.StatusCode
		httpx.DrainAndClose(resp.Body)
		return nil, fmt.Errorf("stream continuation upstream returned status %d", status)
	}
	continued := 0
	if r.meta != nil {
		r.meta.Continued++
		r.meta.Legs = append(r.meta.Legs, "cont:resume")
		continued = r.meta.Continued
	}
	r.g.logger.Info("stream continued on a new exit", "component", "upstream", "event", "stream_continued", "request_id", r.ids.Request, "model", r.route.ID, "partial_bytes", len(partial), "resume_attempt", continued)
	return resp.Body, nil
}

// buildContinuationBody appends the delivered partial plus the continuation
// instruction to the original Zen body. A trailing assistant message is
// merged in place so the request never carries two adjacent assistant turns;
// every other field (tools, stream options, effort, ...) is preserved, and
// prepareAnonymousBody re-asserts the anonymous free-tier shape.
func buildContinuationBody(zenBody []byte, partial string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(zenBody, &payload); err != nil {
		return nil, fmt.Errorf("parse continuation base body: %w", err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("continuation base body has no messages")
	}
	if last, ok := messages[len(messages)-1].(map[string]any); ok && last["role"] == "assistant" {
		content, ok := last["content"].(string)
		if !ok {
			return nil, errors.New("trailing assistant message has non-string content")
		}
		last["content"] = content + partial
	} else {
		messages = append(messages, map[string]any{"role": "assistant", "content": partial})
	}
	messages = append(messages, map[string]any{"role": "user", "content": continuationInstruction})
	payload["messages"] = messages
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("continuation body contains unsupported JSON values")
	}
	return prepareAnonymousBody(encoded, wire.Chat), nil
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
