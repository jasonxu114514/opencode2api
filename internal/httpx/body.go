package httpx

import (
	"io"
)

// DrainAndClose closes body without speculative reads. A byte-limited drain
// is not time-limited: even one unread byte can stall the caller forever.
// Callers that want connection reuse must consume the body under their own
// deadline first; the regular and Bun transports already reuse a connection
// after a clean EOF. Closing an unread body only gives up reuse. The name is
// kept for existing callers. A nil body is ignored.
func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_ = body.Close()
}
