package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/config"
)

// errFirstEventTimeout and errAttemptTimeout are the cancel causes of the two
// deadlines doInferenceAttempt arms. Both wrap context.DeadlineExceeded, so
// generic timeout checks still match them.
var (
	errFirstEventTimeout = fmt.Errorf("upstream first SSE event timed out: %w", context.DeadlineExceeded)
	errAttemptTimeout    = fmt.Errorf("upstream attempt timed out: %w", context.DeadlineExceeded)
)

// attemptTrace records how far a single attempt got before it failed. The
// shared ResponseHeaderTimeout only covers the header wait; this trace tells
// apart the phases it cannot see (dial, request write) from the ones it can.
// The callbacks run on transport goroutines, so the flags are atomic.
type attemptTrace struct {
	gotConn      atomic.Bool
	wroteRequest atomic.Bool
	gotFirstByte atomic.Bool
}

// phase names the phase the attempt is in: "dial" until a connection is
// obtained, "write" until the request is written, "headers" until the first
// response byte arrives, and "first_event" afterwards.
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

// errProxyUnavailable marks an attempt that never reached the upstream because
// the proxy route itself failed. When it is the final error of a request, the
// client gets a proxy error instead of a generic bad gateway.
var errProxyUnavailable = errors.New("proxy unavailable")

// proxyUnavailableError is an attempt error that asProxyUnavailable attributed
// to the proxy route. It matches both errProxyUnavailable and the original
// error under errors.Is and errors.As.
type proxyUnavailableError struct {
	proxy string
	err   error
}

func (e *proxyUnavailableError) Error() string {
	return fmt.Sprintf("proxy %s unavailable: %s", e.proxy, e.err.Error())
}
func (e *proxyUnavailableError) Unwrap() []error { return []error{errProxyUnavailable, e.err} }

// asProxyUnavailable tags an attempt error that belongs to the proxy route
// rather than the upstream: one that died before a connection to the upstream
// existed (the "dial" phase), or a *net.OpError raised by the proxy dialer
// itself (Op "proxyconnect", or one starting with "socks"). Through a real
// proxy the dial phase covers the proxy dial, the CONNECT/SOCKS handshake and
// the TLS handshake tunnelled through it, so the failure belongs to the proxy
// rather than the upstream. Direct transports are left untouched: there a dial
// failure is an upstream reachability problem.
func asProxyUnavailable(proxy *proxyTransport, err error) error {
	if err == nil || proxy == nil || proxy.name == "direct" {
		return err
	}
	var pe *phasedError
	var op *net.OpError
	if errors.As(err, &pe) && pe.phase == "dial" || errors.As(err, &op) && (op.Op == "proxyconnect" || strings.HasPrefix(op.Op, "socks")) {
		return &proxyUnavailableError{proxy: config.RedactURL(proxy.name), err: err}
	}
	return err
}

// classifyAttemptError renders a leg token for the request log. The phase is
// appended for the attempt-deadline, generic timeout and transport cases so a
// hung exit can be attributed without grepping logs; a first-event timeout is
// its own phase and is reported as plain "first_event".
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

// attemptBody is the response body doInferenceAttempt hands back. Reading
// replays any buffered SSE prelude before the rest of the stream, and Close
// also releases the attempt's context.
type attemptBody struct {
	io.Reader
	body   io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b *attemptBody) Close() error {
	b.cancel(nil)
	return b.body.Close()
}

// doInferenceAttempt sends one upstream attempt through client under two
// optional deadlines. It passes straight through to client.Do when both
// timeouts are zero or negative, and for the /systemone path.
//
// firstEventTimeout applies only to a 2xx text/event-stream response: before
// returning it to the retry loop, while nothing has been committed downstream,
// it waits for the first complete SSE event (see readFirstEvent). Every byte
// read meanwhile is kept and replayed for native forwarding. Comments and
// incomplete frames do not count as the first event. Any other response is
// returned as soon as its headers arrive.
//
// attemptTimeout bounds the whole attempt (dial + request write + response
// headers + first event). ResponseHeaderTimeout only covers the header wait,
// so a wedged exit can otherwise hang in the write or dial phase until the
// request-level budget is gone. Once the response is committed the timer can
// no longer cut the established stream.
//
// Outside the pass-through cases, failures are returned as a *phasedError
// naming the phase the attempt reached, with errAttemptTimeout or
// errFirstEventTimeout as the error when that deadline caused the failure, and
// closing the returned body also releases the attempt's context.
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

// readFirstEvent reads up to and including the blank line that ends the first
// SSE event carrying a data field, and returns every byte read for replay.
// Comment lines and data-less events before it stay in the returned prefix but
// do not end the wait. It fails if the stream ends first (io.ErrUnexpectedEOF),
// on a read error, or once the prelude would exceed 1 MiB.
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
