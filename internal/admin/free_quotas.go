package admin

import (
	"net/http"

	"opencode2api/internal/httpx"
)

// handleFreeQuotas returns the current UTC day's estimated free-model
// allowance plus every configured egress joined with its tracker state. There
// is no upstream remaining-quota endpoint, so the snapshot is a local estimate
// learned from observed attempts and quota 429 responses.
func (a *Server) handleFreeQuotas(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, a.manager.FreeQuotaReport())
}
