package protocol

import (
	"net/http"

	"opencode2api/internal/httpx"
)

// WriteError writes a JSON error response with the given status in the
// envelope the protocol's clients expect: {"type":"error","error":{...}} for
// Anthropic, and the OpenAI {"error":{message,type,param,code}} shape for
// every other protocol, System One included. kind becomes the error type, and
// a non-empty requestID is echoed in the x-request-id header.
func WriteError(w http.ResponseWriter, protocol Protocol, status int, message, kind, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("x-request-id", requestID)
	}
	if protocol == Anthropic {
		httpx.WriteJSONStatus(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
		return
	}
	httpx.WriteJSONStatus(w, status, map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": nil}})
}
