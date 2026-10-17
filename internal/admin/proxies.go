package admin

import (
	"net/http"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

// The UI fans out a batch as individual checks so progress and cancellation
// remain responsive even when several proxies time out.
func (a *Server) handleCheckProxy(w http.ResponseWriter, r *http.Request) {
	var input SecretInput
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	current := a.manager.Config()
	values, err := resolveSecrets([]SecretInput{input}, current.Proxies)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		return
	}
	// Validate drafts with the same rules as saved proxies, without reading a
	// proxyfile or persisting any configuration changes.
	current.Proxies, current.ProxyFile = values, ""
	if _, err := config.Normalize("", current); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		return
	}
	select {
	case a.proxyChecks <- struct{}{}:
		defer func() { <-a.proxyChecks }()
	default:
		writeAdminError(w, http.StatusTooManyRequests, "proxy_checks_busy", "too many proxy checks; retry when the current checks finish")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	result, err := a.manager.CheckProxy(r.Context(), values[0])
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "proxy_check_unavailable", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}
