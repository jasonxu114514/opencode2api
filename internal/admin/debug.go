package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/httpx"
	"opencode2api/internal/jsonutil"
	modelcatalog "opencode2api/internal/models"
	"opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// DebugKeySelection tells the Playground whether to use normal routing or to
// exercise one explicitly chosen upstream key.
type DebugKeySelection struct {
	Mode string      `json:"mode,omitempty"`
	Tier config.Tier `json:"tier,omitempty"`
	ID   string      `json:"id,omitempty"`
}

type DebugInferenceRequest struct {
	Protocol protocol.Protocol `json:"protocol"`
	Key      DebugKeySelection `json:"key,omitempty"`
	Request  map[string]any    `json:"request"`
}

type DebugInferenceResult struct {
	OK          bool                         `json:"ok"`
	HTTPStatus  int                          `json:"http_status"`
	DurationMS  int64                        `json:"duration_ms"`
	RequestID   string                       `json:"request_id,omitempty"`
	Route       modelcatalog.RouteDiagnostic `json:"route"`
	SelectedKey *gateway.DebugKeyView        `json:"selected_key,omitempty"`
	KeyTest     string                       `json:"key_test,omitempty"`
	Response    any                          `json:"response"`
}

func (a *Server) handleDebugModels(w http.ResponseWriter, _ *http.Request) {
	models, metadata := a.manager.DebugModels()
	catalog := a.manager.Resources().Models
	keys := a.manager.DebugKeys()
	a.mu.Lock()
	last := a.lastInference
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": models, "keys": keys, "metadata": metadata, "catalog": catalog, "last_inference": last})
}

func (a *Server) handleDebugInference(w http.ResponseWriter, r *http.Request) {
	if !a.allowDebug(clientIP(r)) {
		writeAdminError(w, http.StatusTooManyRequests, "debug_rate_limited", "too many Playground requests; retry in one minute")
		return
	}
	var input DebugInferenceRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !protocol.Valid(input.Protocol) {
		writeAdminError(w, http.StatusBadRequest, "invalid_protocol", "protocol must be chat, responses, or anthropic")
		return
	}
	if input.Request == nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request must be a JSON object")
		return
	}
	payload := jsonutil.CloneMap(input.Request)
	payload["stream"] = false
	model := jsonutil.StringAt(payload, "model")
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request.model is required")
		return
	}
	if input.Key.Mode == "" {
		input.Key.Mode = "auto"
	}
	if input.Key.Mode != "auto" && input.Key.Mode != "selected" {
		writeAdminError(w, http.StatusBadRequest, "invalid_key_mode", "key.mode must be auto or selected")
		return
	}
	var selectedKey *gateway.DebugKeyView
	if input.Key.Mode == "selected" {
		selectedKey = a.manager.DebugKey(input.Key.Tier, input.Key.ID)
		if selectedKey == nil {
			writeAdminError(w, http.StatusBadRequest, "unknown_key_id", "selected key is not configured")
			return
		}
	}
	route := a.manager.DebugRoute(model, input.Protocol)
	if selectedKey != nil {
		selected, routeErr := a.manager.DebugRouteForTier(model, selectedKey.Tier)
		if routeErr != nil {
			writeAdminError(w, http.StatusBadRequest, "model_unavailable_for_key_tier", routeErr.Error())
			return
		}
		route = selected
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "request contains unsupported JSON values")
		return
	}
	path := protocol.Path(input.Protocol)
	trace := &telemetry.RequestMeta{}
	ctx := telemetry.WithRequestMeta(gateway.WithDiagnosticRequest(r.Context()), trace)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gateway.local"+path, bytes.NewReader(encoded))
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "debug_request_failed", "could not construct Gateway request")
		return
	}
	cfg := a.manager.Config()
	if cfg.FirstEnabledServerKey() == "" {
		writeAdminError(w, http.StatusServiceUnavailable, "debug_unavailable", "no local server key is configured")
		return
	}
	request.Header.Set("Authorization", "Bearer "+cfg.FirstEnabledServerKey())
	if selectedKey != nil {
		request = request.WithContext(gateway.WithDebugKeyOverride(request.Context(), gateway.DebugKeyOverride{Tier: selectedKey.Tier, KeyID: selectedKey.ID}))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	recorder := newDebugResponseRecorder()
	started := time.Now()
	a.manager.Handler().ServeHTTP(recorder, request)
	duration := time.Since(started)
	requestID := recorder.Header().Get("x-request-id")
	if trace.Channel != "" {
		route.Anonymous = trace.Anonymous
		route.Tier = config.Tier(trace.Tier)
		route.KeyID = trace.KeyID
		route.Channel = trace.Channel
		route.Attempts = trace.Attempts
		route.NativeProtocol = trace.Protocol
	}
	var raw any
	if json.Unmarshal(recorder.body.Bytes(), &raw) != nil {
		raw = recorder.body.String()
	}
	raw = sanitizeDebugValue(raw, a.manager.Redact)
	keyTest := ""
	if selectedKey != nil {
		if current := a.manager.DebugKey(selectedKey.Tier, selectedKey.ID); current != nil {
			selectedKey = current
		}
		keyTest = classifyKeyTest(recorder.status, trace.AttemptOutcome)
	}
	result := DebugInferenceResult{
		OK: recorder.status >= 200 && recorder.status < 300, HTTPStatus: recorder.status,
		DurationMS: max(duration.Milliseconds(), 0), RequestID: requestID, Route: route,
		SelectedKey: selectedKey, KeyTest: keyTest, Response: raw,
	}
	a.mu.Lock()
	a.lastInference = &result
	a.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, result)
}

type debugResponseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newDebugResponseRecorder() *debugResponseRecorder {
	return &debugResponseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (recorder *debugResponseRecorder) Header() http.Header { return recorder.header }

func (recorder *debugResponseRecorder) WriteHeader(status int) {
	if recorder.status != http.StatusOK || status == http.StatusOK {
		return
	}
	recorder.status = status
}

func (recorder *debugResponseRecorder) Write(data []byte) (int, error) {
	return recorder.body.Write(data)
}

func sanitizeDebugValue(value any, redact func(string) string) any {
	switch current := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, item := range current {
			lower := strings.ToLower(key)
			if sensitiveDebugKey(lower) {
				result[key] = "***"
				continue
			}
			result[key] = sanitizeDebugValue(item, redact)
		}
		return result
	case []any:
		result := make([]any, len(current))
		for index, item := range current {
			result[index] = sanitizeDebugValue(item, redact)
		}
		return result
	case string:
		return redact(current)
	default:
		return value
	}
}

func (a *Server) allowDebug(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	window := a.debugAttempts[client]
	if window.Started.IsZero() || now.Sub(window.Started) >= time.Minute {
		window = loginWindow{Started: now}
	}
	if window.Count >= 12 {
		return false
	}
	window.Count++
	a.debugAttempts[client] = window
	if len(a.debugAttempts) > 4096 {
		for key, candidate := range a.debugAttempts {
			if now.Sub(candidate.Started) >= time.Minute {
				delete(a.debugAttempts, key)
			}
		}
	}
	return true
}

// classifyKeyTest turns a per-key diagnostic into a verdict about that key.
// Only statuses that are unambiguous about the credential are reported as key
// problems: a 4xx request-shape error says nothing about the key, and a 5xx may
// be entirely unrelated to it.
func classifyKeyTest(status int, attemptOutcome string) string {
	switch {
	case attemptOutcome == "transport_error":
		return "transport_error"
	case status >= 200 && status < 300:
		return "usable"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "rejected"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == 0:
		return "unavailable"
	case status >= 500:
		return "upstream_error"
	default:
		return "request_error"
	}
}

func sensitiveDebugKey(key string) bool {
	for _, hint := range []string{
		"authorization", "cookie", "password", "secret", "api_key", "api-key", "x-api-key",
		"access_token", "refresh_token", "set-cookie", "credential",
	} {
		if strings.Contains(key, hint) {
			return true
		}
	}
	return false
}
