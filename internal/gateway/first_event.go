package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var errFirstEventTimeout = fmt.Errorf("upstream first SSE event timed out: %w", context.DeadlineExceeded)
var errBodyIdleTimeout = errors.New("upstream SSE body idle timeout")

type attemptBody struct {
	io.Reader
	body   io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *attemptBody) Close() error {
	b.cancel(nil)
	return b.body.Close()
}

// Wait before returning a successful SSE response to the retry loop, while
// nothing has been committed downstream. Keep every byte for native forwarding.
// Comments and incomplete frames do not count as the first event.
// idleTimeoutBody prevents an established SSE stream from hanging forever
// after the first event. It closes the underlying response when no body bytes
// arrive within timeout; the blocked Read then returns the dedicated sentinel.
type idleTimeoutBody struct {
	body     io.ReadCloser
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
	closed   bool
}

func newIdleTimeoutBody(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if timeout <= 0 || body == nil {
		return body
	}
	return &idleTimeoutBody{body: body, timeout: timeout}
}

func (b *idleTimeoutBody) armLocked() {
	if b.timeout <= 0 || b.closed || b.timedOut {
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.timeout, b.fire)
		return
	}
	b.timer.Reset(b.timeout)
}

func (b *idleTimeoutBody) fire() {
	b.mu.Lock()
	if b.closed || b.timedOut {
		b.mu.Unlock()
		return
	}
	b.timedOut = true
	b.mu.Unlock()
	_ = b.body.Close()
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.armLocked()
	b.mu.Unlock()

	n, err := b.body.Read(p)

	b.mu.Lock()
	timedOut := b.timedOut
	if err != nil {
		if b.timer != nil {
			b.timer.Stop()
		}
	} else if !timedOut && !b.closed {
		b.armLocked()
	}
	b.mu.Unlock()

	if timedOut {
		return 0, errBodyIdleTimeout
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	return b.body.Close()
}

func doInferenceAttempt(client *http.Client, req *http.Request, timeout time.Duration, bodyIdleTimeout time.Duration) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/systemone") {
		return client.Do(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := client.Do(req.Clone(ctx))
	if err != nil {
		cancel(nil)
		return resp, err
	}
	if resp.StatusCode/100 != 2 || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = &attemptBody{Reader: resp.Body, body: resp.Body, cancel: cancel}
		return resp, nil
	}
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { cancel(errFirstEventTimeout) })
	}
	reader := bufio.NewReader(resp.Body)
	prefix, err := readFirstEvent(reader)
	if timer != nil {
		stopped := timer.Stop()
		if !stopped && ctx.Err() == nil {
			// Stop(false) means the callback may be about to run concurrently.
			cancel(errFirstEventTimeout)
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		cancel(nil)
		resp.Body.Close()
		return nil, err
	}
	body := &attemptBody{Reader: io.MultiReader(bytes.NewReader(prefix), reader), body: resp.Body, cancel: cancel}
	resp.Body = newIdleTimeoutBody(body, bodyIdleTimeout)
	return resp, nil
}

func readFirstEvent(reader *bufio.Reader) ([]byte, error) {
	const maxPrelude = 1 << 20
	var prefix, line []byte
	hasData := false
	for {
		part, err := reader.ReadSlice('\n')
		if len(prefix)+len(part) > maxPrelude {
			return nil, errors.New("upstream SSE prelude exceeds 1 MiB")
		}
		prefix = append(prefix, part...)
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		line = line[:0]
		if text == "" && hasData {
			return prefix, nil
		}
		if strings.HasPrefix(text, "data:") || text == "data" {
			hasData = true
		}
	}
}
