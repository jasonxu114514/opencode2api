package admin

import (
	"net/http"

	"opencode2api/internal/httpx"
)

// handleFreeQuotas returns the current UTC day's free-model usage per egress.
// The upstream exposes no remaining-quota endpoint, so every number is a local
// estimate learned from traffic this gateway already sent.
func (a *Server) handleFreeQuotas(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, a.manager.FreeQuotas())
}
