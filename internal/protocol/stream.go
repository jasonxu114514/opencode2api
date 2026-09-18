package protocol

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

var errStreamNormalTermination = errors.New("upstream stream terminated normally")

var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

// ErrZeroDeliveryReset signals a mid-stream upstream reset before anything
// reached downstream. The error frame is deliberately NOT emitted: the
// gateway replays the turn on a fresh attempt inside the same connection.
var ErrZeroDeliveryReset = errors.New("upstream reset before delivery")

type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

type bridgeStreamEvent struct {
	Kind       string
	ResponseID string
	Model      string
	Text       string
	Signature  string
	ToolKey    string
	ToolID     string
	ToolName   string
	Stop       string
	Error      string
	ErrorType  string
	Encrypted  string
	Usage      *Usage
}

func transcodeStream(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) error {
	_, _, err := transcodeStreamWithUsage(w, reader, from, to, model)
	return err
}

func transcodeStreamWithUsage(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	usage, reported, _, err := TranscodeStream(context.Background(), w, reader, from, to, model)
	return usage, reported, err
}

// StreamOutcome carries the terminal state of a transcoded stream for the
// request log and the zero-delivery replay decision.
type StreamOutcome struct {
	Stop string
	Tail string
	// Delivered is false when the stream died before anything reached
	// downstream. The gateway uses it for the zero-delivery replay.
	Delivered bool
}

// EmitStreamError writes a terminal upstream-error frame for a caller that
// took over a stream (e.g. after a failed zero-delivery replay). Headers
// are already sent, so this is SSE framing, not an HTTP status.
func EmitStreamError(w http.ResponseWriter, to Protocol, model string, cause error) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support streaming")
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	return emitUnexpectedStreamError(emitter, cause)
}

// TranscodeStream is the request-aware form used by the
// gateway. A cancelled client must not receive a synthetic upstream error
// after its connection has gone away.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, StreamOutcome, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, StreamOutcome{}, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := newBridgeStreamEmitter(w, flusher, to, model)
	termination := streamOpen
	readErr := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			termination = streamErrorTermination
			if emitErr := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: err.Error(), ErrorType: "upstream_error"}); emitErr != nil {
				return emitErr
			}
			return errStreamUpstreamFailure
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				termination = streamNormalTermination
			case "error":
				termination = streamErrorTermination
			}
			if err := emitter.Emit(event); err != nil {
				return err
			}
			if event.Kind == "done" {
				return errStreamNormalTermination
			}
		}
		return nil
	})
	outcome := func() StreamOutcome {
		stop := emitter.StopReason()
		out := StreamOutcome{Stop: stop, Delivered: emitter.Delivered()}
		if (stop == "length" || stop == "") && emitter.TextLen() > 0 {
			out.Tail = emitter.TextTail(200)
		}
		return out
	}
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, outcome(), nil
		}
		if ClientCanceled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, outcome(), readErr
		}
		if termination == streamOpen {
			// Zero-delivery reset: nothing reached downstream yet, so do
			// NOT emit the error frame. The gateway replays the turn on
			// a fresh attempt; emitting first would poison the stream
			// and make the replay unreceivable.
			if !emitter.Delivered() {
				return emitter.usage, emitter.usageReported, outcome(), ErrZeroDeliveryReset
			}
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, outcome(), emitErr
			}
		}
		return emitter.usage, emitter.usageReported, outcome(), readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, outcome(), nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, outcome(), errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, outcome(), ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, outcome(), err
	}
	return emitter.usage, emitter.usageReported, outcome(), errSSEUnexpectedEOF
}

func emitUnexpectedStreamError(emitter *bridgeStreamEmitter, cause error) error {
	message := "upstream SSE stream ended before a terminal event"
	if cause != nil && !errors.Is(cause, errSSEUnexpectedEOF) {
		message = fmt.Sprintf("upstream SSE stream failed: %v", cause)
	}
	err := emitter.Emit(bridgeStreamEvent{Kind: "error", Error: message, ErrorType: "upstream_error"})
	if errors.Is(err, errStreamUpstreamFailure) {
		return nil
	}
	return err
}

func ClientCanceled(ctx context.Context, streamErr error) bool {
	if errors.Is(streamErr, context.Canceled) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// sseFlushWriter preserves an upstream SSE byte stream while making each
// successful write visible to the client immediately. io.Copy is free to
// choose large writes, so flushing in Write is the only reliable place to
// keep same-protocol streams live.
type sseFlushWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (writer *sseFlushWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if n > 0 {
		writer.flusher.Flush()
	}
	return n, err
}

func forwardSSEWithUsage(w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	return ForwardStream(context.Background(), w, reader, protocol, model)
}

func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	_, copyErr := io.Copy(&sseFlushWriter{writer: w, flusher: flusher}, io.TeeReader(reader, observer))
	usage := observer.Finish()
	if observer.ErrorTermination() {
		if copyErr != nil {
			return usage, observer.Reported(), copyErr
		}
		return usage, observer.Reported(), errStreamUpstreamFailure
	}
	if observer.NormalTermination() && observer.ParseError() == nil {
		return usage, observer.Reported(), copyErr
	}
	if ClientCanceled(ctx, copyErr) {
		if copyErr == nil {
			copyErr = ctx.Err()
		}
		return usage, observer.Reported(), copyErr
	}
	cause := observer.ParseError()
	if copyErr != nil {
		cause = copyErr
	}
	if cause == nil {
		cause = errSSEUnexpectedEOF
	}
	emitter := newBridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), err
	}
	return usage, observer.Reported(), cause
}

type streamUsageObserver struct {
	parser   *bridgeStreamParser
	buffer   []byte
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
}

func newStreamUsageObserver(protocol Protocol) *streamUsageObserver {
	return &streamUsageObserver{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

func (observer *streamUsageObserver) Write(data []byte) (int, error) {
	observer.buffer = append(observer.buffer, data...)
	for {
		index, width := nextSSEBoundary(observer.buffer)
		if index < 0 {
			break
		}
		frame := append([]byte(nil), observer.buffer[:index+width]...)
		observer.buffer = observer.buffer[index+width:]
		observer.consume(frame)
	}
	return len(data), nil
}

func (observer *streamUsageObserver) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageObserver) Reported() bool { return observer.reported }

func (observer *streamUsageObserver) NormalTermination() bool { return observer.normal }

func (observer *streamUsageObserver) ErrorTermination() bool { return observer.error }

func (observer *streamUsageObserver) ParseError() error { return observer.parseErr }

func (observer *streamUsageObserver) consume(frame []byte) {
	_ = readSSE(strings.NewReader(string(frame)), func(eventName, data string) error {
		events, err := observer.parser.Parse(eventName, data)
		if err != nil {
			if observer.parseErr == nil {
				observer.parseErr = err
			}
			return nil
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				observer.normal = true
			case "error":
				observer.error = true
			}
			if event.Usage != nil {
				observer.reported = true
				mergeBridgeUsage(&observer.usage, *event.Usage)
			}
		}
		return nil
	})
}

func nextSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

func readSSE(reader io.Reader, handler func(eventName, data string) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var eventName string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		err := handler(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return err
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(dataLines) > 0 {
		// A blank line is the SSE record delimiter. Do not parse a final
		// unterminated record as a complete frame; an EOF without a terminal
		// event is handled by the caller as a truncated stream.
		return errSSEUnexpectedEOF
	}
	return nil
}
