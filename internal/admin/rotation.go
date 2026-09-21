package admin

import (
	"fmt"
	"net/http"
	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"slices"
)

func (a *Server) handleRotation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, 200, a.manager.RotationSnapshot())
}
func (a *Server) handleSaveRotation(w http.ResponseWriter, r *http.Request) {
	var input config.RotationConfig
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		if err := input.Validate(nil); err != nil {
			return err
		}
		state := a.manager.RotationSnapshot()
		for i, g := range state.Groups {
			order := input.SOTA.Order
			if i == 1 {
				order = input.Sweet.Order
			}
			if len(order) != len(g.Order) {
				return fmt.Errorf("模型列表已更新，请刷新后重新排序")
			}
			for _, v := range order {
				if !slices.Contains(g.Order, v) {
					return fmt.Errorf("只能调整组内模型的顺序")
				}
			}
		}
		cfg.Rotation = input
		return nil
	})
	if err != nil {
		writeAdminError(w, 400, "rotation_rejected", a.manager.Redact(err.Error()))
		return
	}
	a.handleRotation(w, r)
}
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
