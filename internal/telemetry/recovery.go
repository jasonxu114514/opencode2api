package telemetry

import (
	"log/slog"
	"net/http"

	"opencode2api/internal/protocol"
)

// Recover turns a panic in next into a logged error and a 500 response in the
// Chat error format.
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				logger.Error("request handler panicked", "component", "http", "event", "request_panic", "error", value)
				protocol.WriteError(w, protocol.Chat, http.StatusInternalServerError, "internal server error", "server_error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
