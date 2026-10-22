package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
)

func TestProxyCheckAuthenticationAndInput(t *testing.T) {
	a := New(&gateway.RuntimeManager{}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.sessions[tokenDigest("session")] = adminSession{
		AuthVersion: config.Fingerprint(""), CSRF: "csrf", Expires: time.Now().Add(time.Hour),
	}
	for _, tc := range []struct {
		name, body, csrf, origin string
		cookie                   bool
		want                     int
	}{
		{"anonymous", `{}`, "", "", false, 401},
		{"missing csrf", `{}`, "", "", true, 403},
		{"foreign origin", `{}`, "csrf", "https://other.example", true, 403},
		{"malformed body", `{`, "csrf", "", true, 400},
		{"empty input", `{}`, "csrf", "", true, 400},
		{"stale saved proxy", `{"id":"missing"}`, "csrf", "", true, 400},
		{"unknown fields", `{"url":"http://proxy.example"}`, "csrf", "", true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/proxies/check", strings.NewReader(tc.body))
			if tc.cookie {
				request.AddCookie(&http.Cookie{Name: adminCookieName, Value: "session"})
			}
			request.Header.Set("X-CSRF-Token", tc.csrf)
			request.Header.Set("Origin", tc.origin)
			response := httptest.NewRecorder()
			a.Handler().ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}
