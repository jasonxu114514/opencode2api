package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"opencode2api/internal/jsonutil"
)

// errStreamUpstreamFailure marks a stream that failed in a way already
// reported, or, in CollapseStream, already recorded, so the caller must not
// report it a second time.
var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")

// errStreamNormalTermination reports that the stream reached its own terminal
// event, so the caller treats it as a success.
var errStreamNormalTermination = errors.New("upstream stream terminated normally")

// errSSEUnexpectedEOF reports a stream that ended without a terminal event.
var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

// streamTermination records how a transcoded stream ended, so the code after
// readSSE can tell a normal terminal event from an upstream error.
type streamTermination uint8

const (
	streamOpen streamTermination = iota
	streamNormalTermination
	streamErrorTermination
)

// bridgeStreamEvent is one protocol-neutral streaming event, parsed from an
// upstream stream and encoded for the downstream protocol. Kind selects which
// fields matter: "start" (ResponseID, Model), "text", "reasoning" (Text and/or
// Encrypted), "reasoning_signature" (Signature), "tool_start" and "tool_delta"
// (ToolKey, ToolID, ToolName, Text), "usage", "finish" (Stop), "error"
// (Error, ErrorType) and "done".
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

// TranscodeStream parses an upstream stream of one protocol and re-encodes it
// for another, writing each downstream event as it arrives. It returns the
// usage the emitter collected and whether the upstream reported any.
//
// A nil error means the stream reached its terminal event. Otherwise the error
// is errStreamUpstreamFailure when the failure was already reported in-band,
// the error that ended the read, or ctx.Err() when the client was canceled
// after the stream itself ended cleanly. A stream that ends without a terminal
// event is reported in-band with an error event first, using
// errSSEUnexpectedEOF when nothing more specific is known.
//
// A canceled client gets no synthetic upstream error event: the gateway
// cancels the request context when the client goes away, and writing after
// that only wastes the remaining attempt.
func TranscodeStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	parser := &bridgeStreamParser{
		protocol:          from,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	emitter := bridgeStreamEmitter(w, flusher, to, model)
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
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) {
			return emitter.usage, emitter.usageReported, nil
		}
		if ClientCanceled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, readErr
		}
		if termination == streamOpen {
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, emitErr
			}
		}
		return emitter.usage, emitter.usageReported, readErr
	}
	if termination == streamNormalTermination {
		return emitter.usage, emitter.usageReported, nil
	}
	if termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, errStreamUpstreamFailure
	}
	if ClientCanceled(ctx, nil) {
		return emitter.usage, emitter.usageReported, ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, err
	}
	return emitter.usage, emitter.usageReported, errSSEUnexpectedEOF
}

// emitUnexpectedStreamError reports a stream that ended without a terminal
// event, as an in-band error event in the downstream protocol. It returns nil
// once that event is written, and the write error otherwise.
func emitUnexpectedStreamError(emitter *bridgeStreamEncoder, cause error) error {
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

// ClientCanceled reports whether a stream error is the client going away,
// either through the stream error itself or through the request context.
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

// ForwardStream relays a same-protocol upstream stream to the client byte for
// byte, flushing each write, while a usage tracker reads the same bytes
// through a TeeReader. It returns the tracked usage and whether the upstream
// reported any, plus an error: the copy error, if any, for a normal
// termination that parsed cleanly; otherwise errStreamUpstreamFailure when an
// upstream error event was relayed, the copy error, or the parse error (or
// errSSEUnexpectedEOF) after an error event was emitted for a stream that
// ended early. A canceled client gets no synthetic error event.
func ForwardStream(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (Usage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Usage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := streamUsageObserver(protocol)
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
	emitter := bridgeStreamEmitter(w, flusher, protocol, model)
	if err := emitUnexpectedStreamError(emitter, cause); err != nil {
		return usage, observer.Reported(), err
	}
	return usage, observer.Reported(), cause
}

// RenameStreamReader wraps an SSE stream reader and rewrites aliased tool call
// names back to the names the client declared before downstream parsers or
// writers consume the stream. An empty rename set returns reader unchanged.
//
// It is a pull-based transform instead of a goroutine plus io.Pipe so a client
// that disconnects mid-stream cannot strand a blocked writer. Chat frames go
// through the fragment-aware rewriter, while the other protocols use the
// stateless RenameResponseTool rewrite on each frame.
func RenameStreamReader(reader io.Reader, protocol Protocol, renames *ToolRenames) io.Reader {
	if renames.Empty() {
		return reader
	}
	return &renameStreamReader{
		buffer:  bufio.NewReaderSize(reader, 64<<10),
		renamer: &streamToolRenamer{protocol: protocol, renames: renames, tools: map[string]*streamToolRenameState{}},
	}
}

// renameStreamReader is the io.Reader RenameStreamReader returns: it reads the
// upstream stream line by line, rewrites the data lines that need it, and
// serves the rewritten bytes, carrying an error over to the next Read.
type renameStreamReader struct {
	buffer  *bufio.Reader
	renamer *streamToolRenamer
	pending []byte
	err     error
}

// Read returns the rewritten stream bytes, buffering one line at a time.
func (reader *renameStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			return 0, reader.err
		}
		// ReadString either returns a non-empty chunk with a nil error or
		// returns the trailing chunk together with EOF, so one iteration
		// always either fills pending or reports the error below; the loop
		// condition only guards the entry.
		line, err := reader.buffer.ReadString('\n')
		if len(line) > 0 {
			reader.pending = reader.rewriteLine(line)
		}
		if err != nil {
			// The final chunk is drained before the error is reported so a
			// truncated stream still delivers every byte the upstream sent.
			reader.err = err
			if len(reader.pending) == 0 {
				return 0, err
			}
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// rewriteLine transforms one complete SSE line, preserving its original bytes
// and terminator whenever nothing needs to be rewritten.
func (reader *renameStreamReader) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	if raw == "" || raw == "[DONE]" {
		return []byte(line)
	}
	rewritten := reader.renamer.rewriteFrame(raw)
	if string(rewritten) == raw {
		return []byte(line)
	}
	return []byte("data: " + string(rewritten) + terminator)
}

// streamToolRenameState tracks how one tool call's name was resolved while a
// stream is rewritten. Chat deltas may repeat a growing name prefix or append
// fragments, so a value that is still a prefix of a known wire name is held
// back until it either completes the match or diverges.
type streamToolRenameState struct {
	decided bool
	renamed bool
	pending string
}

// streamToolRenamer rewrites the tool names of one protocol's SSE frames,
// keeping per-tool state across frames so a name split over several Chat
// chunks is reassembled before it is matched.
type streamToolRenamer struct {
	protocol Protocol
	renames  *ToolRenames
	tools    map[string]*streamToolRenameState
}

// rewriteFrame rewrites one SSE data payload, which is raw JSON text. Frames
// that cannot carry a tool name are returned untouched, so only a payload that
// mentions a client alias (Anthropic and Responses) or "tool_calls" (Chat) is
// decoded.
func (renamer *streamToolRenamer) rewriteFrame(raw string) []byte {
	if renamer.protocol != Chat {
		// Anthropic and Responses streams always carry a complete tool name in
		// a single event, so the stateless object rewrite is exact.
		if !mayCarryToolName([]byte(raw)) {
			return []byte(raw)
		}
		return RenameResponseTool([]byte(raw), renamer.protocol, renamer.renames)
	}
	if !strings.Contains(raw, `"tool_calls"`) {
		return []byte(raw)
	}
	return renamer.rewriteChatFrame(raw)
}

// rewriteChatFrame rewrites the tool names of one Chat chunk. Tool calls are
// keyed by index, then id, then array position, and each name fragment is fed
// through applyNameFragment so a name split across chunks is restored as a
// whole. It returns the frame unchanged when nothing was rewritten.
func (renamer *streamToolRenamer) rewriteChatFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	changed := false
	for _, rawChoice := range jsonutil.SliceAt(payload, "choices") {
		choice, _ := rawChoice.(map[string]any)
		for _, container := range []string{"delta", "message"} {
			message, _ := choice[container].(map[string]any)
			if message == nil {
				continue
			}
			for callIndex, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				if function == nil {
					continue
				}
				state := renamer.toolState(fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id"), callIndex)))
				if name, ok := function["name"].(string); ok && name != "" {
					if renamer.applyNameFragment(state, function, name) {
						changed = true
					}
				}
				if _, ok := function["arguments"]; ok && !state.decided {
					// Arguments can only start once the name is complete: flush
					// whatever was held back, verbatim.
					if state.pending != "" {
						function["name"] = state.pending
						changed = true
					}
					state.decided = true
				}
			}
		}
	}
	if !changed {
		return []byte(raw)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(raw)
	}
	return encoded
}

// applyNameFragment folds one name fragment into a tool's rename state and
// reports whether the frame changed. Once the accumulated name matches a wire
// name exactly, it is replaced by the client name; while it is still a proper
// prefix of one the name is held back; on divergence the accumulated name is
// written out as is. After a resolved rename, any further fragment is dropped,
// because it belongs to a longer name the client never declared.
func (renamer *streamToolRenamer) applyNameFragment(state *streamToolRenameState, function map[string]any, fragment string) bool {
	if state.decided {
		if !state.renamed {
			return false
		}
		// A resolved alias accepts no further fragments: any continuation
		// belongs to a longer name the client never declared.
		if _, exists := function["name"]; exists {
			delete(function, "name")
			return true
		}
		return false
	}
	state.pending = mergeToolName(state.pending, fragment)
	if client, ok := renamer.renames.Client(state.pending); ok {
		function["name"] = client
		state.decided, state.renamed = true, true
		return true
	}
	if renamer.renames.HasLongerPrefix(state.pending) {
		// Still a prefix of a known wire name: hold the fragment until a later
		// frame either completes the match or diverges.
		delete(function, "name")
		return true
	}
	// The name is complete. Resolve folds a spelling that differs from exactly
	// one declared client tool only in case or separators; a name nothing
	// declared is written out as the model sent it.
	client, renamed := renamer.renames.Resolve(state.pending)
	function["name"] = client
	state.decided, state.renamed = true, renamed
	return true
}

// toolState returns the rename state for a tool key, creating it on first
// sight.
func (renamer *streamToolRenamer) toolState(key string) *streamToolRenameState {
	if state := renamer.tools[key]; state != nil {
		return state
	}
	state := &streamToolRenameState{}
	renamer.tools[key] = state
	return state
}

// streamUsageTracker is an io.Writer that splits a relayed stream into SSE
// frames and parses them for usage, while recording whether the stream reached
// a terminal or error event and whether any frame failed to parse. It never
// modifies the bytes it is given.
type streamUsageTracker struct {
	parser   *bridgeStreamParser
	buffer   []byte
	usage    Usage
	reported bool
	normal   bool
	error    bool
	parseErr error
}

// streamUsageObserver builds a usage tracker whose parser reads the given
// protocol.
func streamUsageObserver(protocol Protocol) *streamUsageTracker {
	return &streamUsageTracker{parser: &bridgeStreamParser{
		protocol: protocol, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{},
		responseArgs: map[string]bool{}, responseReasoning: map[string]bool{},
	}}
}

// Write buffers the relayed bytes and consumes every complete SSE frame they
// contain, keeping the incomplete tail for the next call. It never fails.
func (observer *streamUsageTracker) Write(data []byte) (int, error) {
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

// Finish returns the merged usage and discards the buffered tail.
func (observer *streamUsageTracker) Finish() Usage {
	// An unterminated final line is not an SSE frame. In particular, do not
	// count a usage object from a response that was cut off at EOF.
	observer.buffer = nil
	return observer.usage
}

func (observer *streamUsageTracker) Reported() bool { return observer.reported }

func (observer *streamUsageTracker) NormalTermination() bool { return observer.normal }

func (observer *streamUsageTracker) ErrorTermination() bool { return observer.error }

func (observer *streamUsageTracker) ParseError() error { return observer.parseErr }

// consume parses one complete SSE frame and records its terminal events and
// usage. A parse error is remembered but not propagated, because the relay
// must keep copying bytes.
func (observer *streamUsageTracker) consume(frame []byte) {
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

// nextSSEBoundary finds the end of the first complete SSE frame in data: it
// returns the index where the frame's terminator starts and the terminator's
// width, preferring whichever of a blank line ("\n\n") or a CRLF blank line
// ("\r\n\r\n") comes first. No complete frame yields -1.
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

// readSSE reads a stream and calls handler once per complete SSE record, with
// the record's event name and its data lines joined by newlines. A record ends
// at a blank line; comment lines are skipped, and a record without any data
// line is not delivered. A final record whose data is not terminated by a
// blank line is an unexpected EOF rather than a complete event. Scanner
// errors, and errors returned by handler, end the read.
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
