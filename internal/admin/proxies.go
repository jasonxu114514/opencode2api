package admin

import (
	"net/http"
	"strings"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

func (a *Server) handleCheckProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input SecretInput
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "provide exactly one proxy id or value")
		return
	}
	input.ID = strings.TrimSpace(input.ID)
	input.Value = strings.TrimSpace(input.Value)
	if (input.ID == "") == (input.Value == "") || len(input.Value) > 16<<10 {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxy", "provide exactly one proxy id or value of at most 16 KiB")
		return
	}
	value := input.Value
	if input.ID != "" {
		for _, existing := range a.manager.Config().Proxies {
			if config.Fingerprint(existing) == input.ID {
				value = existing
				break
			}
		}
		if value == "" {
			writeAdminError(w, http.StatusBadRequest, "unknown_proxy_id", "proxy id is unknown or stale; reload the configuration")
			return
		}
	}
	if err := config.ValidateProxyEntry(value); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_proxy", "proxy must be direct or an HTTP, HTTPS, SOCKS5 or SOCKS5H URL")
		return
	}
	select {
	case a.proxyChecks <- struct{}{}:
		defer func() { <-a.proxyChecks }()
	case <-r.Context().Done():
		return
	default:
		w.Header().Set("Retry-After", "1")
		writeAdminError(w, http.StatusTooManyRequests, "proxy_checks_busy", "too many proxy checks; retry shortly")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a.manager.CheckProxy(r.Context(), value))
}
