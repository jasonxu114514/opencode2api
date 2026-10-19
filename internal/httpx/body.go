package httpx

import (
	"io"
)

// DrainAndClose discards up to 64 KiB of body and closes it, so the
// connection can be reused. A nil body is ignored. The bound keeps a small
// response fully drained while a large or endless one cannot hold the caller
// for more than that amount of data.
func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}
