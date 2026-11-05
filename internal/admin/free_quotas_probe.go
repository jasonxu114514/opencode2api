package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/freequota"
	"opencode2api/internal/gateway"
	"opencode2api/internal/httpx"
	"opencode2api/internal/telemetry"
)

// FreeQuotaProbeRequest selects one egress or every egress for an active
// free-model probe. Relay names a configured proxy; Warp probes the WARP lane;
// All probes every configured proxy and the WARP lane once.
type FreeQuotaProbeRequest struct {
	Relay gateway.RelaySelection `json:"relay,omitempty"`
	Warp  bool                   `json:"warp,omitempty"`
	All   bool                   `json:"all,omitempty"`
	Model string                 `json:"model,omitempty"`
}

// FreeQuotaProbeResult reports what one probe request actually observed.
// Classification uses the same rule as the tracker, so a probe that receives
// a genuine daily-quota 429 marks the same scope the live traffic would.
type FreeQuotaProbeResult struct {
	Index          int       `json:"index"`
	Kind           string    `json:"kind"`
	Label          string    `json:"label"`
	OK             bool      `json:"ok"`
	Status         int       `json:"status"`
	LatencyMS      int64     `json:"latency_ms"`
	Model          string    `json:"model"`
	Classification string    `json:"classification"`
	Error          string    `json:"error"`
	QuotaExhausted bool      `json:"quota_exhausted"`
	ProbedAt       time.Time `json:"probed_at"`
}

type freeQuotaProbeTarget struct {
	Index int
	Kind  string
	Label string
	Relay gateway.RelaySelection
	Warp  bool
}

// handleFreeQuotaProbe sends one minimal free-model request through each
// selected egress and returns the fresh report. Diagnostic requests never
// change proxy health, cooldowns, or the anonymous breaker.
func (a *Server) handleFreeQuotaProbe(w http.ResponseWriter, r *http.Request) {
	if !a.allowDebug(clientIP(r)) {
		writeAdminError(w, http.StatusTooManyRequests, "debug_rate_limited", "too many probe requests; retry in one minute")
		return
	}
	var input FreeQuotaProbeRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	report := a.manager.FreeQuotaReport()
	model, ok := a.manager.FreeProbeModel(input.Model)
	if !ok {
		if input.Model != "" {
			writeAdminError(w, http.StatusBadRequest, "not_a_free_model", "model is not a routable free model")
		} else {
			writeAdminError(w, http.StatusServiceUnavailable, "no_free_model", "no free model is currently routable")
		}
		return
	}
	targets, err := freeQuotaProbeTargets(report, input)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_probe_target", err.Error())
		return
	}

	results := make([]FreeQuotaProbeResult, len(targets))
	workers := min(len(targets), 4)
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				index := next
				next++
				mu.Unlock()
				if index >= len(targets) {
					return
				}
				results[index] = a.runFreeQuotaProbe(r.Context(), model, targets[index])
			}
		}()
	}
	wg.Wait()

	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"probes": results, "report": a.manager.FreeQuotaReport()})
}

func freeQuotaProbeTargets(report gateway.FreeQuotaReport, input FreeQuotaProbeRequest) ([]freeQuotaProbeTarget, error) {
	if input.All {
		targets := make([]freeQuotaProbeTarget, 0, len(report.Egresses)+1)
		for _, egress := range report.Egresses {
			if !egress.ProbeSupported || egress.Index < 0 {
				continue
			}
			index := egress.Index
			targets = append(targets, freeQuotaProbeTarget{
				Index: egress.Index, Kind: egress.Kind, Label: egress.Label,
				Relay: gateway.RelaySelection{Index: &index},
			})
		}
		if report.Warp != nil && report.Warp.Enabled && report.Warp.Active > 0 {
			targets = append(targets, freeQuotaProbeTarget{Index: -1, Kind: "warp", Label: "warp", Warp: true})
		}
		return targets, nil
	}
	if input.Warp {
		if report.Warp == nil || !report.Warp.Enabled {
			return nil, errors.New("warp is not enabled")
		}
		return []freeQuotaProbeTarget{{Index: -1, Kind: "warp", Label: "warp", Warp: true}}, nil
	}
	if input.Relay.Empty() {
		return nil, errors.New("relay, warp, or all is required")
	}
	for _, egress := range report.Egresses {
		if egress.Kind != "proxy" && egress.Kind != "direct" {
			continue
		}
		if input.Relay.Index != nil && egress.Index != *input.Relay.Index {
			continue
		}
		if input.Relay.Name != "" && egress.Label != input.Relay.Name {
			continue
		}
		index := egress.Index
		return []freeQuotaProbeTarget{{
			Index: egress.Index, Kind: egress.Kind, Label: egress.Label,
			Relay: gateway.RelaySelection{Index: &index},
		}}, nil
	}
	return nil, errors.New("relay does not match a configured proxy")
}

func (a *Server) runFreeQuotaProbe(parent context.Context, model string, target freeQuotaProbeTarget) FreeQuotaProbeResult {
	result := FreeQuotaProbeResult{
		Index: target.Index, Kind: target.Kind, Label: target.Label,
		Model: model, ProbedAt: time.Now().UTC(),
	}
	cfg := a.manager.Config()
	if cfg.FirstEnabledServerKey() == "" {
		result.Classification = "error"
		result.Error = "no local server key is configured"
		return result
	}
	payload := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with the single word ok."}},
		"max_tokens": 4,
		"stream":     false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		result.Classification = "error"
		result.Error = "could not encode the probe request"
		return result
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	trace := &telemetry.RequestMeta{}
	ctx = telemetry.WithRequestMeta(gateway.WithDiagnosticRequest(ctx), trace)
	if target.Warp {
		ctx = gateway.WithWARPProbe(ctx)
	} else {
		ctx = gateway.WithRelayOverride(ctx, target.Relay)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gateway.local/v1/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		result.Classification = "error"
		result.Error = "could not construct the gateway request"
		return result
	}
	request.Header.Set("Authorization", "Bearer "+cfg.FirstEnabledServerKey())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-opencode-session", benchSession())
	recorder := debugResponseRecorder()
	started := time.Now()
	a.manager.Handler().ServeHTTP(recorder, request)
	result.LatencyMS = max(time.Since(started).Milliseconds(), 0)
	result.Status = recorder.status
	result.OK = recorder.status >= 200 && recorder.status < 300
	body := recorder.body.Bytes()
	retryAfter := gateway.ParseRetryAfter(recorder.header.Get("Retry-After"))
	result.Classification = freequota.ClassifyAttempt(result.Status, body, retryAfter, time.Now(), false)
	result.QuotaExhausted = freequota.GenuineQuota(result.Status, body, retryAfter, time.Now())
	if target.Warp && trace.Proxy != "" {
		result.Label = trace.Proxy
	}
	if !result.OK {
		result.Error = probeErrorSnippet(a.manager.Redact, body, result.Status)
	} else if len(body) == 0 {
		result.OK = false
		result.Classification = "error"
		result.Error = "empty response; the probe likely timed out"
	}
	return result
}

func probeErrorSnippet(redact func(string) string, body []byte, status int) string {
	text := strings.TrimSpace(redact(string(body)))
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	if len(text) > 240 {
		text = text[:240]
	}
	return text
}
