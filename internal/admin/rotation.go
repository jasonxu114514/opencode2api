package admin

import (
	"net/http"
	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

// handleRotation reports the rotation state: both groups with their alias,
// order, current model, pause map and recent switches, plus the recent
// attempts and the first-output timeout.
func (a *Server) handleRotation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, 200, a.manager.RotationSnapshot())
}

// handleSaveRotation stores new aliases and orders. Each order must be a
// permutation of the group's current order, so a list the WebUI rendered
// before a catalog refresh is rejected instead of dropping models.
func (a *Server) handleSaveRotation(w http.ResponseWriter, r *http.Request) {
	var input config.RotationConfig
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	input.Defaults()
	if err := input.Validate(nil); err != nil {
		writeAdminError(w, 400, "rotation_rejected", a.manager.Redact(err.Error()))
		return
	}
	if _, err := a.manager.Update(func(cfg *config.Config) error {
		cfg.Rotation = input
		return nil
	}); err != nil {
		writeAdminError(w, 400, "rotation_rejected", a.manager.Redact(err.Error()))
		return
	}
	a.handleRotation(w, r)
}
func (a *Server) handlePreviewRotation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Query config.RotationQuery `json:"query"`
		Sort  *config.GroupSort    `json:"sort"`
		Pin   []string             `json:"pin"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := input.Query.Validate(); err != nil {
		writeAdminError(w, 400, "rotation_rejected", err.Error())
		return
	}
	if input.Sort != nil {
		if err := input.Sort.Validate(); err != nil {
			writeAdminError(w, 400, "rotation_rejected", err.Error())
			return
		}
	}
	facts := a.manager.PreviewRotation(input.Query, input.Sort, input.Pin)
	httpx.WriteJSON(w, 200, map[string]any{"models": facts, "count": len(facts)})
}

// handleSelectRotation switches a group to another of its models manually,
// clearing that model's pause.
func (a *Server) handleSelectRotation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Group string `json:"group"`
		Model string `json:"model"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.manager.SelectRotation(input.Group, input.Model); err != nil {
		writeAdminError(w, 400, "rotation_rejected", a.manager.Redact(err.Error()))
		return
	}
	a.handleRotation(w, r)
}
