package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// StreamFailure classifies a failed upstream stream without putting its raw
// text into the returned error. Message holds the upstream error text so a
// caller can classify the failure (quota wording, for one) without returning
// that text to the client; Type holds the upstream error type. Error reports a
// fixed generic message instead. It is returned for failures both before and
// after a stream is committed, and the gateway inspects it to classify the
// attempt before deciding whether to rotate to another model.
type StreamFailure struct{ Message, Type string }

// Error returns a generic message, keeping the upstream text out of logs and
// error chains that print the error itself.
func (e *StreamFailure) Error() string { return "upstream stream failed" }

// ClientWriteError wraps a failure to write to the downstream client, so the
// gateway can tell a vanished client from an upstream failure. Unwrap exposes
// the underlying write error.
type ClientWriteError struct{ Err error }

// Error returns a generic message; Err carries the detail.
func (e *ClientWriteError) Error() string { return "downstream write failed" }

// Unwrap returns the underlying write error.
func (e *ClientWriteError) Unwrap() error { return e.Err }

// RotationStream streams one upstream response to the client while holding
// back events until the attempt is worth committing to. Upstream events are
// parsed with the parser for from, and the emitter encodes them for to under
// the requested alias.
//
// Events that carry no output (start, usage, finish) are held in pending until
// an effective event arrives: text, reasoning, a reasoning signature, a tool
// start or delta, or done. At that point ready is called (the gateway uses it
// to stop the first-output timer and set the response headers), the attempt is
// marked committed, and the held events are emitted first. If 256 events are
// already held and another non-effective one arrives, the attempt fails with
// invalid_prelude. An upstream error event before that point is returned as a
// StreamFailure so the gateway can rotate to another model; once committed, a
// failure is emitted in-band as an error event and never replayed. The event
// model is always cleared, because the emitter's model is the requested alias
// for the whole response.
//
// It returns the usage the emitter collected and whether the upstream reported
// usage at all, whether the stream was committed, and the stream error: nil
// after a normal termination; a *StreamFailure for an upstream error event, a
// parse error, more than 256 held events, or a stream that ended without a
// terminal event; a *ClientWriteError when writing to the client failed; the
// error returned by ready; or whatever ended the read, errSSEUnexpectedEOF
// included. A client cancellation does not get a synthetic error event.
func RotationStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, alias string, ready func() error) (usage Usage, reported, committed bool, result error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return usage, false, false, fmt.Errorf("streaming is unsupported")
	}
	parser := streamUsageObserver(from).parser
	emitter := bridgeStreamEmitter(w, flusher, to, alias)
	pending := []bridgeStreamEvent{}
	var upstreamFailure error
	emit := func(event bridgeStreamEvent) error {
		event.Model = "" // The emitter's model is the requested alias for the whole response.
		if err := emitter.Emit(event); err != nil {
			if errors.Is(err, errStreamUpstreamFailure) {
				return err
			}
			return &ClientWriteError{err}
		}
		return nil
	}
	readErr := readSSE(reader, func(name, data string) error {
		events, err := parser.Parse(name, data)
		if err != nil {
			return &StreamFailure{Type: "parse_error"}
		}
		for _, event := range events {
			if event.Kind == "error" {
				upstreamFailure = &StreamFailure{event.Error, event.ErrorType}
				return upstreamFailure
			}
			if !committed {
				effective := event.Kind == "text" || event.Kind == "reasoning" || event.Kind == "reasoning_signature" || event.Kind == "tool_start" || event.Kind == "tool_delta" || event.Kind == "done"
				if !effective {
					if len(pending) >= 256 {
						return &StreamFailure{Type: "invalid_prelude"}
					}
					pending = append(pending, event)
					continue
				}
				if err := ready(); err != nil {
					return err
				}
				committed = true
				for _, p := range pending {
					if err := emit(p); err != nil {
						return err
					}
				}
				pending = nil
			}
			if err := emit(event); err != nil {
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	usage, reported = emitter.usage, emitter.usageReported
	if errors.Is(readErr, errStreamNormalTermination) {
		return usage, reported, committed, nil
	}
	if readErr == nil {
		readErr = &StreamFailure{Type: "unexpected_eof"}
	}
	var writeErr *ClientWriteError
	if committed && ctx.Err() == nil && !errors.As(readErr, &writeErr) {
		_ = emit(bridgeStreamEvent{Kind: "error", Error: "当前上游模型的输出中断；本次响应已结束，请重新发起请求。", ErrorType: "upstream_error"})
	}
	return usage, reported, committed, readErr
}
