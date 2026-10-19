package admin

import (
	"net/http"
	"opencode2api/internal/httpx"
)

// handleAvailability lists the free models with their probe state, so the
// availability page can show which ones are disabled and why.
func (a *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": a.manager.AvailabilitySnapshot()})
}

// handleRestoreModel returns a disabled free model to manual control, which
// re-enables it and keeps automatic probes from disabling it again. It answers
// with the refreshed availability list.
func (a *Server) handleRestoreModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model string `json:"model"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.manager.RestoreModel(input.Model); err != nil {
		writeAdminError(w, 400, "restore_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleAvailability(w, r)
}

// handleManualModel switches a free model between manual and automatic
// control: a manual model stays enabled and is not probed, while switching
// back to automatic schedules a probe. It answers with the refreshed list.
func (a *Server) handleManualModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model  string `json:"model"`
		Manual *bool  `json:"manual"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.Manual == nil {
		writeAdminError(w, 400, "invalid_request", "manual is required")
		return
	}
	if err := a.manager.SetManualModel(input.Model, *input.Manual); err != nil {
		writeAdminError(w, 400, "manual_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleAvailability(w, r)
}
