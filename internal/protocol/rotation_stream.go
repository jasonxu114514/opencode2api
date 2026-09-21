package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// StreamFailure carries upstream classification without exposing its raw text
// to the caller or diagnostics. It can be examined before committing a stream.
type StreamFailure struct{ Message, Type string }

func (e *StreamFailure) Error() string { return "upstream stream failed" }

type ClientWriteError struct{ Err error }

func (e *ClientWriteError) Error() string { return "downstream write failed" }
func (e *ClientWriteError) Unwrap() error { return e.Err }

// RotationStream holds role/start/usage events until useful content or a normal
// terminal event. Ready commits the chosen model and stops the first-output
// timer. Once committed, failures are emitted in-band and never replayed.
func RotationStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, alias string, ready func() error) (usage Usage, reported, committed bool, result error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return usage, false, false, fmt.Errorf("streaming is unsupported")
	}
	parser := newStreamUsageObserver(from).parser
	emitter := newBridgeStreamEmitter(w, flusher, to, alias)
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
