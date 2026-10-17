package admin

import (
	"net/http"
	"opencode2api/internal/httpx"
)

func (a *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": a.manager.AvailabilitySnapshot()})
}

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

func (a *Server) handleProbeModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model string `json:"model"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	result, err := a.manager.ProbeNow(input.Model)
	if err != nil {
		writeAdminError(w, 400, "probe_failed", a.manager.Redact(err.Error()))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"probe": result})
}
