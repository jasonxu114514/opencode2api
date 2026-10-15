package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errFirstEventTimeout = fmt.Errorf("upstream first SSE event timed out: %w", context.DeadlineExceeded)
	errAttemptTimeout    = fmt.Errorf("upstream attempt timed out: %w", context.DeadlineExceeded)
)

// attemptTrace records how far a single attempt got before it failed. The
// shared ResponseHeaderTimeout only covers the header wait; this probe tells
// apart the phases it cannot see (dial, request write) from the ones it can.
// The callbacks run on transport goroutines, so the flags are atomic.
type attemptTrace struct {
	gotConn      atomic.Bool
	wroteRequest atomic.Bool
	gotFirstByte atomic.Bool
}

func (t *attemptTrace) phase() string {
	switch {
	case !t.gotConn.Load():
		return "dial"
	case !t.wroteRequest.Load():
		return "write"
	case !t.gotFirstByte.Load():
		return "headers"
	default:
		return "first_event"
	}
}

func (t *attemptTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { t.gotConn.Store(true) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.wroteRequest.Store(true) },
		GotFirstResponseByte: func() { t.gotFirstByte.Store(true) },
	}
}

// upstreamAccepted reports whether an attempt died after the upstream had
// already answered with headers. Such a timeout is slow generation, not a
// broken proxy, so it must not evict the exit.
func upstreamAccepted(err error) bool {
	var pe *phasedError
	return errors.As(err, &pe) && pe.phase == "first_event"
}

// phasedError carries the phase an attempt died in. Unwrap keeps the original
// error, so existing errors.Is checks keep working.
type phasedError struct {
	err   error
	phase string
}

func (e *phasedError) Error() string { return fmt.Sprintf("%s [phase=%s]", e.err.Error(), e.phase) }
func (e *phasedError) Unwrap() error { return e.err }

// classifyAttemptError renders a leg token for the request log. The phase is
// appended for the attempt-deadline and transport cases so a hung exit can be
// attributed without grepping logs.
func classifyAttemptError(err error) string {
	var pe *phasedError
	phase := ""
	if errors.As(err, &pe) {
		phase = "_" + pe.phase
	}
	switch {
	case errors.Is(err, errAttemptTimeout):
		return "attempt_timeout" + phase
	case errors.Is(err, errFirstEventTimeout):
		return "first_event"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout" + phase
	default:
		return "transport" + phase
	}
}

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
//
// attemptTimeout bounds the whole attempt (dial + request write + response
// headers + first event). ResponseHeaderTimeout only covers the header wait,
// so a wedged exit can otherwise hang in the write or dial phase until the
// request-level budget is gone.
func doInferenceAttempt(client *http.Client, req *http.Request, firstEventTimeout, attemptTimeout time.Duration) (*http.Response, error) {
	if (firstEventTimeout <= 0 && attemptTimeout <= 0) || strings.HasSuffix(req.URL.Path, "/systemone") {
		return client.Do(req)
	}
	trace := &attemptTrace{}
	ctx, cancel := context.WithCancelCause(req.Context())
	req = req.Clone(httptrace.WithClientTrace(ctx, trace.clientTrace()))
	var (
		deadlineMu   sync.Mutex
		committed    bool
		attemptTimer *time.Timer
	)
	if attemptTimeout > 0 {
		attemptTimer = time.AfterFunc(attemptTimeout, func() {
			deadlineMu.Lock()
			defer deadlineMu.Unlock()
			if !committed {
				cancel(errAttemptTimeout)
			}
		})
	}
	// commitDeadline decides success against the attempt deadline atomically:
	// either the deadline already fired (cause returned) or the response is
	// committed and the timer can no longer cut an established stream.
	commitDeadline := func() error {
		if attemptTimer == nil {
			return context.Cause(ctx)
		}
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		committed = true
		attemptTimer.Stop()
		return nil
	}
	defer func() {
		deadlineMu.Lock()
		committed = true
		deadlineMu.Unlock()
		if attemptTimer != nil {
			attemptTimer.Stop()
		}
	}()
	resp, err := client.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && errors.Is(cause, errAttemptTimeout) {
			err = cause
		}
		cancel(nil)
		return resp, &phasedError{err: err, phase: trace.phase()}
	}
	// Without a first-event wait the attempt is done once headers arrive.
	if firstEventTimeout <= 0 || resp.StatusCode/100 != 2 || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		if attemptTimer != nil {
			if cause := commitDeadline(); cause != nil {
				cancel(nil)
				resp.Body.Close()
				return nil, &phasedError{err: cause, phase: trace.phase()}
			}
		}
		resp.Body = &attemptBody{Reader: resp.Body, body: resp.Body, cancel: cancel}
		return resp, nil
	}
	timer := time.AfterFunc(firstEventTimeout, func() { cancel(errFirstEventTimeout) })
	reader := bufio.NewReader(resp.Body)
	prefix, err := readFirstEvent(reader)
	stopped := timer.Stop()
	if !stopped && ctx.Err() == nil {
		// Stop(false) means the callback may still be about to run.
		cancel(errFirstEventTimeout)
	}
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		cancel(nil)
		resp.Body.Close()
		return nil, &phasedError{err: err, phase: trace.phase()}
	}
	if cause := commitDeadline(); cause != nil {
		cancel(nil)
		resp.Body.Close()
		return nil, &phasedError{err: cause, phase: trace.phase()}
	}
	resp.Body = &attemptBody{Reader: io.MultiReader(bytes.NewReader(prefix), reader), body: resp.Body, cancel: cancel}
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
