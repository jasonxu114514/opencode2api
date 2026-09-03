package main

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
	"time"
)

var errStreamUpstreamFailure = errors.New("upstream stream failure delivered")
var errStreamNormalTermination = errors.New("upstream stream terminated normally")
var errSSEUnexpectedEOF = errors.New("unexpected end of SSE stream")

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
	Usage      *bridgeUsage
}

type bridgeStreamParser struct {
	protocol          Protocol
	started           bool
	tools             map[string]bool
	toolIDs           map[string]string
	toolNames         map[string]string
	toolOrder         []string
	responseArgs      map[string]bool
	responseReasoning map[string]bool
}

func transcodeStream(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) error {
	_, _, err := transcodeStreamWithUsage(w, reader, from, to, model)
	return err
}

func transcodeStreamWithUsage(w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (bridgeUsage, bool, error) {
	return transcodeStreamWithUsageContext(context.Background(), w, reader, from, to, model)
}

// transcodeStreamWithUsageContext is the request-aware form used by the
// gateway. A cancelled client must not receive a synthetic upstream error
// after its connection has gone away.
func transcodeStreamWithUsageContext(ctx context.Context, w http.ResponseWriter, reader io.Reader, from, to Protocol, model string) (bridgeUsage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return bridgeUsage{}, false, fmt.Errorf("response writer does not support streaming")
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
	if readErr != nil {
		if errors.Is(readErr, errStreamNormalTermination) || errors.Is(readErr, errStreamUpstreamFailure) {
			return emitter.usage, emitter.usageReported, nil
		}
		if streamClientCancelled(ctx, readErr) {
			return emitter.usage, emitter.usageReported, readErr
		}
		if termination == streamOpen {
			if emitErr := emitUnexpectedStreamError(emitter, readErr); emitErr != nil {
				return emitter.usage, emitter.usageReported, emitErr
			}
		}
		return emitter.usage, emitter.usageReported, readErr
	}
	if termination == streamNormalTermination || termination == streamErrorTermination {
		return emitter.usage, emitter.usageReported, nil
	}
	if streamClientCancelled(ctx, nil) {
		return emitter.usage, emitter.usageReported, ctx.Err()
	}
	if err := emitUnexpectedStreamError(emitter, errSSEUnexpectedEOF); err != nil {
		return emitter.usage, emitter.usageReported, err
	}
	return emitter.usage, emitter.usageReported, nil
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

func streamClientCancelled(ctx context.Context, streamErr error) bool {
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

func forwardSSEWithUsage(w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (bridgeUsage, bool, error) {
	return forwardSSEWithUsageContext(context.Background(), w, reader, protocol, model)
}

func forwardSSEWithUsageContext(ctx context.Context, w http.ResponseWriter, reader io.Reader, protocol Protocol, model string) (bridgeUsage, bool, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return bridgeUsage{}, false, fmt.Errorf("response writer does not support streaming")
	}
	observer := newStreamUsageObserver(protocol)
	_, copyErr := io.Copy(&sseFlushWriter{writer: w, flusher: flusher}, io.TeeReader(reader, observer))
	usage := observer.Finish()
	if observer.ErrorTermination() || observer.NormalTermination() && observer.ParseError() == nil {
		return usage, observer.Reported(), copyErr
	}
	if streamClientCancelled(ctx, copyErr) {
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
	return usage, observer.Reported(), copyErr
}

type streamUsageObserver struct {
	parser   *bridgeStreamParser
	buffer   []byte
	usage    bridgeUsage
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

func (observer *streamUsageObserver) Finish() bridgeUsage {
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

func (parser *bridgeStreamParser) Parse(eventName, data string) ([]bridgeStreamEvent, error) {
	if data == "[DONE]" {
		if parser.protocol != ProtocolChat {
			return nil, fmt.Errorf("unexpected [DONE] terminator for %s stream", parser.protocol)
		}
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	switch parser.protocol {
	case ProtocolChat:
		return parser.parseChatEvent(eventName, value), nil
	case ProtocolAnthropic:
		return parser.parseAnthropicEvent(eventName, value)
	case ProtocolResponses:
		return parser.parseResponses(eventName, value), nil
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", parser.protocol)
	}
}

func (parser *bridgeStreamParser) parseChat(value map[string]any) []bridgeStreamEvent {
	return parser.parseChatEvent("", value)
}

func (parser *bridgeStreamParser) parseChatEvent(eventName string, value map[string]any) []bridgeStreamEvent {
	events := make([]bridgeStreamEvent, 0, 4)
	if eventName == "error" {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if rawError, exists := value["error"]; exists && rawError != nil {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if !parser.started {
		if id := stringAt(value, "id"); id != "" {
			parser.started = true
			events = append(events, bridgeStreamEvent{Kind: "start", ResponseID: id, Model: stringAt(value, "model")})
		}
	}
	if usageMap := mapAt(value, "usage"); len(usageMap) > 0 {
		usage := decodeOpenAIUsage(usageMap)
		events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
	}
	for _, raw := range sliceAt(value, "choices") {
		choice, _ := raw.(map[string]any)
		delta := mapAt(choice, "delta")
		if reasoning := firstString(stringAt(delta, "reasoning_content"), stringAt(delta, "reasoning")); reasoning != "" {
			events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: reasoning})
		}
		if text := stringAt(delta, "content"); text != "" {
			events = append(events, bridgeStreamEvent{Kind: "text", Text: text})
		}
		for _, rawCall := range sliceAt(delta, "tool_calls") {
			call, _ := rawCall.(map[string]any)
			key := fmt.Sprint(firstAny(call["index"], stringAt(call, "id")))
			function := mapAt(call, "function")
			id := stringAt(call, "id")
			if _, seen := parser.tools[key]; !seen {
				parser.tools[key] = false
				parser.toolOrder = append(parser.toolOrder, key)
			}
			if id != "" {
				parser.toolIDs[key] = id
			}
			if name := stringAt(function, "name"); name != "" {
				parser.toolNames[key] = mergeToolName(parser.toolNames[key], name)
			}
			if arguments := stringAt(function, "arguments"); arguments != "" {
				if !parser.tools[key] && parser.toolNames[key] != "" {
					parser.tools[key] = true
					events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
				}
				events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: arguments})
			}
		}
		if stop := stringAt(choice, "finish_reason"); stop != "" {
			if isStreamErrorFinish(stop) {
				events = append(events, bridgeStreamEvent{Kind: "error", Error: firstString(stringAt(choice, "error", "message"), "upstream Chat stream failed"), ErrorType: "upstream_error"})
				continue
			}
			for _, key := range parser.toolOrder {
				if !parser.tools[key] && parser.toolNames[key] != "" {
					parser.tools[key] = true
					events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
				}
			}
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
	}
	return events
}

func (parser *bridgeStreamParser) parseAnthropic(value map[string]any) ([]bridgeStreamEvent, error) {
	return parser.parseAnthropicEvent("", value)
}

func (parser *bridgeStreamParser) parseAnthropicEvent(eventName string, value map[string]any) ([]bridgeStreamEvent, error) {
	typeName := stringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "message_start":
		message := mapAt(value, "message")
		events := []bridgeStreamEvent{{Kind: "start", ResponseID: stringAt(message, "id"), Model: stringAt(message, "model")}}
		if usageMap := mapAt(message, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return events, nil
	case "content_block_start":
		block := mapAt(value, "content_block")
		key := fmt.Sprint(value["index"])
		switch stringAt(block, "type") {
		case "thinking":
			events := make([]bridgeStreamEvent, 0, 2)
			if thinking := stringAt(block, "thinking"); thinking != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: thinking})
			}
			if signature := stringAt(block, "signature"); signature != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning_signature", Signature: signature})
			}
			return events, nil
		case "redacted_thinking":
			return []bridgeStreamEvent{{Kind: "reasoning", Encrypted: stringAt(block, "data")}}, nil
		case "tool_use":
			parser.tools[key] = true
			return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: stringAt(block, "id"), ToolName: stringAt(block, "name")}}, nil
		case "text":
			if text := stringAt(block, "text"); text != "" {
				return []bridgeStreamEvent{{Kind: "text", Text: text}}, nil
			}
		}
	case "content_block_delta":
		delta := mapAt(value, "delta")
		key := fmt.Sprint(value["index"])
		switch stringAt(delta, "type") {
		case "thinking_delta":
			return []bridgeStreamEvent{{Kind: "reasoning", Text: stringAt(delta, "thinking")}}, nil
		case "signature_delta":
			return []bridgeStreamEvent{{Kind: "reasoning_signature", Signature: stringAt(delta, "signature")}}, nil
		case "text_delta":
			return []bridgeStreamEvent{{Kind: "text", Text: stringAt(delta, "text")}}, nil
		case "input_json_delta":
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, Text: stringAt(delta, "partial_json")}}, nil
		}
	case "message_delta":
		events := make([]bridgeStreamEvent, 0, 2)
		if usageMap := mapAt(value, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		if stop := stringAt(value, "delta", "stop_reason"); stop != "" {
			if isStreamErrorFinish(stop) {
				events = append(events, bridgeStreamEvent{Kind: "error", Error: "upstream Anthropic stream failed", ErrorType: "upstream_error"})
				return events, nil
			}
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
		return events, nil
	case "message_stop":
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	case "error":
		message := firstString(stringAt(value, "error", "message"), "upstream Anthropic stream error")
		return []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: firstString(stringAt(value, "error", "type"), "upstream_error")}}, nil
	}
	return nil, nil
}

func (parser *bridgeStreamParser) parseResponses(eventName string, value map[string]any) []bridgeStreamEvent {
	typeName := stringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "error":
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Responses stream error"), ErrorType: streamErrorType(value)}}
	case "response.created":
		response := mapAt(value, "response")
		return []bridgeStreamEvent{{Kind: "start", ResponseID: stringAt(response, "id"), Model: stringAt(response, "model")}}
	case "response.output_text.delta":
		return []bridgeStreamEvent{{Kind: "text", Text: stringAt(value, "delta")}}
	case "response.reasoning_summary_text.delta":
		key := responseToolKey(value, nil)
		parser.responseReasoning[key] = true
		return []bridgeStreamEvent{{Kind: "reasoning", Text: stringAt(value, "delta")}}
	case "response.output_item.added":
		item := mapAt(value, "item")
		if stringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, stringAt(item, "call_id"), stringAt(item, "name"))
			if parser.toolNames[key] != "" {
				parser.tools[key] = true
				return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]}}
			}
		}
	case "response.function_call_arguments.delta":
		key := responseToolKey(value, nil)
		parser.responseArgs[key] = true
		return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: stringAt(value, "delta")}}
	case "response.function_call_arguments.done":
		key := responseToolKey(value, nil)
		if !parser.responseArgs[key] {
			parser.responseArgs[key] = true
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: stringAt(value, "arguments")}}
		}
	case "response.output_item.done":
		item := mapAt(value, "item")
		if stringAt(item, "type") == "reasoning" {
			key := responseToolKey(value, item)
			if parser.responseReasoning[key] {
				return nil
			}
			blocks := decodeResponsesReasoning(item)
			if len(blocks) > 0 {
				return []bridgeStreamEvent{{Kind: "reasoning", Text: blocks[0].Text, Encrypted: blocks[0].Encrypted}}
			}
			return nil
		}
		if stringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, stringAt(item, "call_id"), stringAt(item, "name"))
			events := make([]bridgeStreamEvent, 0, 2)
			if !parser.tools[key] {
				parser.tools[key] = true
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
			}
			if !parser.responseArgs[key] {
				if arguments := stringAt(item, "arguments"); arguments != "" {
					events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, Text: arguments})
				}
			}
			return events
		}
	case "response.completed", "response.incomplete", "response.failed", "response.done":
		response := mapAt(value, "response")
		usageMap := mapAt(response, "usage")
		if typeName == "response.failed" || stringAt(response, "status") == "failed" {
			message := firstString(stringAt(response, "error", "message"), stringAt(value, "error", "message"), "upstream Responses request failed")
			events := []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: firstString(stringAt(response, "error", "code"), "upstream_error")}}
			if len(usageMap) > 0 {
				usage := decodeOpenAIUsage(usageMap)
				events = append([]bridgeStreamEvent{{Kind: "usage", Usage: &usage}}, events...)
			}
			return events
		}
		stop := "stop"
		if typeName == "response.incomplete" {
			stop = canonicalResponsesIncomplete(stringAt(response, "incomplete_details", "reason"))
		}
		events := make([]bridgeStreamEvent, 0, 3)
		if len(usageMap) > 0 {
			usage := decodeOpenAIUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return append(events, bridgeStreamEvent{Kind: "finish", Stop: stop}, bridgeStreamEvent{Kind: "done"})
	}
	return nil
}

func streamErrorMessage(value map[string]any, fallback string) string {
	if message := stringAt(value, "error", "message"); message != "" {
		return message
	}
	if message := stringAt(value, "message"); message != "" {
		return message
	}
	if message, ok := value["error"].(string); ok && message != "" {
		return message
	}
	return fallback
}

func streamErrorType(value map[string]any) string {
	return firstString(stringAt(value, "error", "type"), stringAt(value, "error", "code"), "upstream_error")
}

func isStreamErrorFinish(stop string) bool {
	switch strings.ToLower(strings.TrimSpace(stop)) {
	case "error", "network_error", "server_error":
		return true
	default:
		return false
	}
}

func (parser *bridgeStreamParser) rememberTool(key, id, name string) {
	if _, seen := parser.tools[key]; !seen {
		parser.tools[key] = false
		parser.toolOrder = append(parser.toolOrder, key)
	}
	if id != "" {
		parser.toolIDs[key] = id
	}
	if name != "" {
		parser.toolNames[key] = name
	}
}

func mergeToolName(current, fragment string) string {
	if current == "" || fragment == current {
		return fragment
	}
	if strings.HasPrefix(fragment, current) {
		return fragment
	}
	return current + fragment
}

func responseToolKey(event map[string]any, item map[string]any) string {
	if item != nil {
		if id := stringAt(item, "id"); id != "" {
			return id
		}
	}
	if id := stringAt(event, "item_id"); id != "" {
		return id
	}
	return fmt.Sprint(event["output_index"])
}

type bridgeStreamTool struct {
	Key              string
	ID               string
	ItemID           string
	Name             string
	Arguments        strings.Builder
	Index            int
	AnthropicIndex   int
	Started          bool
	EmittedArguments int
}

type bridgeStreamEmitter struct {
	w                  io.Writer
	flush              http.Flusher
	target             Protocol
	model              string
	id                 string
	created            int64
	started            bool
	done               bool
	stop               string
	usage              bridgeUsage
	usageReported      bool
	reasoningEncrypted string
	text               strings.Builder
	reasoning          strings.Builder
	reasoningSignature strings.Builder

	tools map[string]*bridgeStreamTool
	order []string

	sequence        int
	nextOutput      int
	textOpen        bool
	reasoningOpen   bool
	reasoningClosed bool
	reasoningItemID string
	reasoningOutput int
	reasoningIndex  int
	textIndex       int
	nextAnthropic   int
	textItemID      string
	textOutput      int
	responseOutput  []any
}

func newBridgeStreamEmitter(writer io.Writer, flusher http.Flusher, target Protocol, model string) *bridgeStreamEmitter {
	return &bridgeStreamEmitter{
		w:       writer,
		flush:   flusher,
		target:  target,
		model:   model,
		created: time.Now().Unix(),
		tools:   map[string]*bridgeStreamTool{},
	}
}

func (emitter *bridgeStreamEmitter) Emit(event bridgeStreamEvent) error {
	if event.ResponseID != "" && emitter.id == "" {
		emitter.id = event.ResponseID
	}
	if event.Model != "" {
		emitter.model = event.Model
	}
	if !emitter.started && (event.Kind == "start" || event.Kind == "reasoning" || event.Kind == "reasoning_signature" || event.Kind == "text" || event.Kind == "tool_start" || event.Kind == "tool_delta") {
		if err := emitter.start(); err != nil {
			return err
		}
	}
	switch event.Kind {
	case "reasoning":
		if event.Encrypted != "" {
			emitter.reasoningEncrypted = event.Encrypted
			if emitter.target != ProtocolResponses && event.Text == "" {
				event.Text = anthropicRedactedThinkingPlaceholder
			}
			if emitter.target == ProtocolResponses && event.Text == "" {
				return emitter.startReasoning()
			}
		}
		if event.Text == "" {
			return nil
		}
		emitter.reasoning.WriteString(event.Text)
		return emitter.emitReasoning(event.Text)
	case "reasoning_signature":
		if event.Signature == "" {
			return nil
		}
		emitter.reasoningSignature.WriteString(event.Signature)
		return emitter.emitReasoningSignature(event.Signature)
	case "text":
		if event.Text == "" {
			return nil
		}
		emitter.text.WriteString(event.Text)
		return emitter.emitText(event.Text)
	case "tool_start":
		tool := emitter.tool(event.ToolKey)
		if event.ToolID != "" {
			tool.ID = event.ToolID
		}
		if event.ToolName != "" {
			tool.Name = event.ToolName
		}
		if err := emitter.startTool(tool); err != nil {
			return err
		}
		return emitter.emitPendingToolArguments(tool)
	case "tool_delta":
		tool := emitter.tool(event.ToolKey)
		if event.ToolID != "" {
			tool.ID = event.ToolID
		}
		if event.ToolName != "" {
			tool.Name = event.ToolName
		}
		tool.Arguments.WriteString(event.Text)
		if err := emitter.startTool(tool); err != nil {
			return err
		}
		return emitter.emitPendingToolArguments(tool)
	case "usage":
		if event.Usage != nil {
			emitter.usageReported = true
			mergeBridgeUsage(&emitter.usage, *event.Usage)
		}
	case "finish":
		emitter.stop = event.Stop
	case "error":
		emitter.done = true
		if err := emitter.emitError(firstString(event.Error, "upstream stream failed"), firstString(event.ErrorType, "upstream_error")); err != nil {
			return err
		}
		return errStreamUpstreamFailure
	case "done":
		return emitter.Finish()
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitError(message, errorType string) error {
	switch emitter.target {
	case ProtocolAnthropic:
		return emitter.sse("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errorType, "message": message},
		})
	case ProtocolResponses:
		response := emitter.responsesBase("failed", []any{})
		response["error"] = map[string]any{"code": errorType, "message": message}
		return emitter.sse("response.failed", map[string]any{
			"type":            "response.failed",
			"response":        response,
			"sequence_number": emitter.nextSequence(),
		})
	default:
		if err := emitter.sse("", map[string]any{"error": map[string]any{
			"message": message,
			"type":    errorType,
			"param":   nil,
			"code":    nil,
		}}); err != nil {
			return err
		}
		return emitter.rawSSE("", "[DONE]")
	}
}

func (emitter *bridgeStreamEmitter) tool(key string) *bridgeStreamTool {
	if key == "" {
		key = fmt.Sprintf("tool-%d", len(emitter.order))
	}
	if current := emitter.tools[key]; current != nil {
		return current
	}
	tool := &bridgeStreamTool{
		Key:    key,
		ID:     randomID("call", 12),
		ItemID: randomID("fc", 12),
		Index:  len(emitter.order),
	}
	emitter.tools[key] = tool
	emitter.order = append(emitter.order, key)
	return tool
}

func (emitter *bridgeStreamEmitter) start() error {
	if emitter.started {
		return nil
	}
	emitter.started = true
	if emitter.id == "" {
		emitter.id = randomID("resp", 12)
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.sse("", map[string]any{
			"id":      asPrefix(emitter.id, "chatcmpl"),
			"object":  "chat.completion.chunk",
			"created": emitter.created,
			"model":   emitter.model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": ""},
				"finish_reason": nil,
			}},
		})
	case ProtocolAnthropic:
		return emitter.sse("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            asPrefix(emitter.id, "msg"),
				"type":          "message",
				"role":          "assistant",
				"model":         emitter.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         anthropicUsage(bridgeUsage{}),
			},
		})
	case ProtocolResponses:
		response := emitter.responsesBase("in_progress", []any{})
		if err := emitter.sse("response.created", map[string]any{"type": "response.created", "response": response, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		return emitter.sse("response.in_progress", map[string]any{"type": "response.in_progress", "response": response, "sequence_number": emitter.nextSequence()})
	default:
		return fmt.Errorf("unsupported stream target %q", emitter.target)
	}
}

func (emitter *bridgeStreamEmitter) emitText(delta string) error {
	if emitter.target == ProtocolAnthropic || emitter.target == ProtocolResponses {
		if err := emitter.finishReasoning(); err != nil {
			return err
		}
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"content": delta}, nil)
	case ProtocolAnthropic:
		if !emitter.textOpen {
			emitter.textOpen = true
			emitter.textIndex = emitter.nextAnthropic
			emitter.nextAnthropic++
			if err := emitter.sse("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         emitter.textIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": emitter.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": delta},
		})
	case ProtocolResponses:
		if !emitter.textOpen {
			emitter.textOpen = true
			emitter.textItemID = randomID("msg", 12)
			emitter.textOutput = emitter.nextOutput
			emitter.nextOutput++
			item := map[string]any{"id": emitter.textItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
			if err := emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": emitter.textOutput, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
				return err
			}
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
			if err := emitter.sse("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "part": part, "sequence_number": emitter.nextSequence()}); err != nil {
				return err
			}
		}
		return emitter.sse("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": delta, "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitReasoning(delta string) error {
	if err := emitter.startReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"reasoning_content": delta}, nil)
	case ProtocolAnthropic:
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": emitter.reasoningIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})
	case ProtocolResponses:
		return emitter.sse("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "delta": delta, "item_id": emitter.reasoningItemID, "output_index": emitter.reasoningOutput, "summary_index": 0, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitReasoningSignature(signature string) error {
	if err := emitter.startReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"reasoning_signature": signature}, nil)
	case ProtocolAnthropic:
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": emitter.reasoningIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": signature},
		})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) startReasoning() error {
	if emitter.reasoningOpen || emitter.reasoningClosed {
		return nil
	}
	emitter.reasoningOpen = true
	switch emitter.target {
	case ProtocolAnthropic:
		emitter.reasoningIndex = emitter.nextAnthropic
		emitter.nextAnthropic++
		return emitter.sse("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         emitter.reasoningIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
	case ProtocolResponses:
		emitter.reasoningItemID = randomID("rs", 12)
		emitter.reasoningOutput = emitter.nextOutput
		emitter.nextOutput++
		item := map[string]any{"id": emitter.reasoningItemID, "type": "reasoning", "status": "in_progress", "summary": []any{}}
		if err := emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": emitter.reasoningOutput, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		part := map[string]any{"type": "summary_text", "text": ""}
		return emitter.sse("response.reasoning_summary_part.added", map[string]any{"type": "response.reasoning_summary_part.added", "item_id": emitter.reasoningItemID, "output_index": emitter.reasoningOutput, "summary_index": 0, "part": part, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) finishReasoning() error {
	if !emitter.reasoningOpen || emitter.reasoningClosed {
		return nil
	}
	emitter.reasoningClosed = true
	text := emitter.reasoning.String()
	switch emitter.target {
	case ProtocolAnthropic:
		return emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": emitter.reasoningIndex})
	case ProtocolResponses:
		if err := emitter.sse("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "text": text, "item_id": emitter.reasoningItemID, "output_index": emitter.reasoningOutput, "summary_index": 0, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		part := map[string]any{"type": "summary_text", "text": text}
		if err := emitter.sse("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "item_id": emitter.reasoningItemID, "output_index": emitter.reasoningOutput, "summary_index": 0, "part": part, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		item := map[string]any{"id": emitter.reasoningItemID, "type": "reasoning", "status": "completed", "summary": []any{part}}
		if emitter.reasoningEncrypted != "" {
			item["encrypted_content"] = emitter.reasoningEncrypted
		}
		return emitter.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": emitter.reasoningOutput, "item": item, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) startTool(tool *bridgeStreamTool) error {
	if tool.Started || tool.Name == "" {
		return nil
	}
	if emitter.target == ProtocolResponses || emitter.target == ProtocolAnthropic {
		if err := emitter.finishReasoning(); err != nil {
			return err
		}
	}
	if emitter.target == ProtocolAnthropic && emitter.textOpen {
		if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": emitter.textIndex}); err != nil {
			return err
		}
		emitter.textOpen = false
	}
	tool.Started = true
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": tool.Index,
			"id":    tool.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      tool.Name,
				"arguments": "",
			},
		}}}, nil)
	case ProtocolResponses:
		tool.Index = emitter.nextOutput
		emitter.nextOutput++
		item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "in_progress", "arguments": "", "call_id": tool.ID, "name": tool.Name}
		return emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.Index, "item": item, "sequence_number": emitter.nextSequence()})
	case ProtocolAnthropic:
		tool.AnthropicIndex = emitter.nextAnthropic
		emitter.nextAnthropic++
		return emitter.sse("content_block_start", map[string]any{
			"type": "content_block_start", "index": tool.AnthropicIndex,
			"content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}},
		})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitToolDelta(tool *bridgeStreamTool, delta string) error {
	if delta == "" {
		return nil
	}
	switch emitter.target {
	case ProtocolChat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    tool.Index,
			"function": map[string]any{"arguments": delta},
		}}}, nil)
	case ProtocolResponses:
		return emitter.sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "delta": delta, "item_id": tool.ItemID, "output_index": tool.Index, "sequence_number": emitter.nextSequence()})
	case ProtocolAnthropic:
		return emitter.sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": tool.AnthropicIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) emitPendingToolArguments(tool *bridgeStreamTool) error {
	if !tool.Started {
		return nil
	}
	arguments := tool.Arguments.String()
	if tool.EmittedArguments >= len(arguments) {
		return nil
	}
	delta := arguments[tool.EmittedArguments:]
	tool.EmittedArguments = len(arguments)
	return emitter.emitToolDelta(tool, delta)
}

func (emitter *bridgeStreamEmitter) Finish() error {
	if emitter.done {
		return nil
	}
	emitter.done = true
	if !emitter.started {
		if err := emitter.start(); err != nil {
			return err
		}
	}
	if emitter.stop == "" {
		if len(emitter.order) > 0 {
			emitter.stop = "tool_calls"
		} else {
			emitter.stop = "stop"
		}
	}
	if err := emitter.finishReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case ProtocolChat:
		if err := emitter.chatChunk(map[string]any{}, chatStop(emitter.stop)); err != nil {
			return err
		}
		if err := emitter.sse("", map[string]any{
			"id":      asPrefix(emitter.id, "chatcmpl"),
			"object":  "chat.completion.chunk",
			"created": emitter.created,
			"model":   emitter.model,
			"choices": []any{},
			"usage":   openAIUsage(emitter.usage),
		}); err != nil {
			return err
		}
		return emitter.rawSSE("", "[DONE]")

	case ProtocolAnthropic:
		if emitter.textOpen {
			if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": emitter.textIndex}); err != nil {
				return err
			}
		}
		for _, key := range emitter.order {
			tool := emitter.tools[key]
			if err := emitter.startTool(tool); err != nil {
				return err
			}
			if err := emitter.emitPendingToolArguments(tool); err != nil {
				return err
			}
			if tool.Started {
				if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": tool.AnthropicIndex}); err != nil {
					return err
				}
			}
		}
		if err := emitter.sse("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": anthropicStop(emitter.stop), "stop_sequence": nil},
			"usage": anthropicUsage(emitter.usage),
		}); err != nil {
			return err
		}
		return emitter.sse("message_stop", map[string]any{"type": "message_stop"})

	case ProtocolResponses:
		if err := emitter.finishResponsesItems(); err != nil {
			return err
		}
		response := bridgeResponse{
			ID:        emitter.id,
			Model:     emitter.model,
			Text:      emitter.text.String(),
			Reasoning: []bridgeBlock{{Kind: "reasoning", ID: emitter.reasoningItemID, Text: emitter.reasoning.String(), Signature: emitter.reasoningSignature.String()}},
			Stop:      emitter.stop,
			Usage:     emitter.usage,
			Created:   emitter.created,
		}
		if emitter.reasoning.Len() == 0 && emitter.reasoningSignature.Len() == 0 {
			response.Reasoning = nil
		}
		for _, key := range emitter.order {
			tool := emitter.tools[key]
			response.Tools = append(response.Tools, bridgeBlock{Kind: "tool_call", ID: tool.ID, Name: tool.Name, ArgumentsJSON: tool.Arguments.String()})
		}
		completed := encodeBridgeResponse(ProtocolResponses, response)
		return emitter.sse("response.completed", map[string]any{"type": "response.completed", "response": completed, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

func (emitter *bridgeStreamEmitter) finishResponsesItems() error {
	if emitter.textOpen {
		text := emitter.text.String()
		if err := emitter.sse("response.output_text.done", map[string]any{"type": "response.output_text.done", "text": text, "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		if err := emitter.sse("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": emitter.textItemID, "output_index": emitter.textOutput, "content_index": 0, "part": part, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		item := map[string]any{"id": emitter.textItemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}}
		if err := emitter.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": emitter.textOutput, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
	}
	for _, key := range emitter.order {
		tool := emitter.tools[key]
		arguments := tool.Arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		if err := emitter.startTool(tool); err != nil {
			return err
		}
		if err := emitter.emitPendingToolArguments(tool); err != nil {
			return err
		}
		if err := emitter.sse("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "arguments": arguments, "item_id": tool.ItemID, "output_index": tool.Index, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "completed", "arguments": arguments, "call_id": tool.ID, "name": tool.Name}
		if err := emitter.sse("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.Index, "item": item, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
	}
	return nil
}

func (emitter *bridgeStreamEmitter) chatChunk(delta map[string]any, finish any) error {
	return emitter.sse("", map[string]any{
		"id":      asPrefix(emitter.id, "chatcmpl"),
		"object":  "chat.completion.chunk",
		"created": emitter.created,
		"model":   emitter.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	})
}

func (emitter *bridgeStreamEmitter) responsesBase(status string, output []any) map[string]any {
	return map[string]any{
		"id":         asPrefix(emitter.id, "resp"),
		"object":     "response",
		"created_at": emitter.created,
		"status":     status,
		"model":      emitter.model,
		"output":     output,
		"error":      nil,
	}
}

func (emitter *bridgeStreamEmitter) nextSequence() int {
	sequence := emitter.sequence
	emitter.sequence++
	return sequence
}

func (emitter *bridgeStreamEmitter) sse(eventName string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emitter.rawSSE(eventName, string(data))
}

func (emitter *bridgeStreamEmitter) rawSSE(eventName, data string) error {
	if eventName != "" {
		if _, err := fmt.Fprintf(emitter.w, "event: %s\n", eventName); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(emitter.w, "data: %s\n\n", data); err != nil {
		return err
	}
	emitter.flush.Flush()
	return nil
}

func mergeBridgeUsage(destination *bridgeUsage, source bridgeUsage) {
	if source.Input != 0 {
		destination.Input = source.Input
	}
	if source.Output != 0 {
		destination.Output = source.Output
	}
	if source.Total != 0 {
		destination.Total = max(destination.Total, source.Total)
	}
	if source.Cached != 0 {
		destination.Cached = source.Cached
	}
	if source.CacheCreation != 0 {
		destination.CacheCreation = source.CacheCreation
	}
	if source.Reasoning != 0 {
		destination.Reasoning = source.Reasoning
	}
	destination.Total = max(destination.Total, destination.Input+destination.Output)
}
