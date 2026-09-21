package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode2api/internal/config"
)

func TestDisabledServerKeysRejected(t *testing.T) {
	g := &Gateway{cfg: config.Config{ServerKeys: []string{"active-secret", "disabled-secret"}, ServerKeyMetadata: map[string]config.ServerKeyMetadata{config.Fingerprint("disabled-secret"): {Disabled: true}}}}
	h := g.authenticate(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, header := range []string{"Authorization", "x-api-key"} {
		for _, key := range []string{"active-secret", "disabled-secret", "unknown"} {
			r := httptest.NewRequest("GET", "/v1/models", nil)
			value := key
			if header == "Authorization" {
				value = "Bearer " + key
			}
			r.Header.Set(header, value)
			w := httptest.NewRecorder()
			h(w, r)
			want := 401
			if key == "active-secret" {
				want = 204
			}
			if w.Code != want {
				t.Errorf("%s status %d want %d", header, w.Code, want)
			}
		}
	}
}
