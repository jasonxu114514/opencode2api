package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"opencode2api/internal/jsonutil"
)

// bridgeStreamParser turns one upstream protocol's SSE events into bridge
// stream events, remembering across events which tool calls were announced and
// which arguments or reasoning already arrived. A Chat stream is the only one
// whose tool names can be split across events, so it accumulates them; the
// other protocols report a complete name in one event.
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

// Parse turns one SSE event of the parser's protocol into zero or more bridge
// events. An event the protocol has no meaning for yields none. Invalid JSON
// is an error, and so is a "[DONE]" terminator on a protocol other than Chat,
// which uses it to end the stream.
func (parser *bridgeStreamParser) Parse(eventName, data string) ([]bridgeStreamEvent, error) {
	if data == "[DONE]" {
		if parser.protocol != Chat {
			return nil, fmt.Errorf("unexpected [DONE] terminator for %s stream", parser.protocol)
		}
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	switch parser.protocol {
	case Chat:
		return parser.parseChatEvent(eventName, value), nil
	case Anthropic:
		return parser.parseAnthropicEvent(eventName, value)
	case Responses:
		return parser.parseResponses(eventName, value), nil
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", parser.protocol)
	}
}

// parseChatEvent turns one Chat chunk into bridge events: an error envelope or
// error finish_reason becomes an error event, the first chunk supplies the
// response ID and model, usage is reported as it appears, and each choice
// contributes reasoning, text, tool and finish events. A tool call whose
// arguments arrive is started once its name is known, and a call whose name is
// known but whose arguments never arrived is started at finish_reason.
func (parser *bridgeStreamParser) parseChatEvent(eventName string, value map[string]any) []bridgeStreamEvent {
	events := make([]bridgeStreamEvent, 0, 4)
	if eventName == "error" {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if rawError, exists := value["error"]; exists && rawError != nil {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if !parser.started {
		if id := jsonutil.StringAt(value, "id"); id != "" {
			parser.started = true
			events = append(events, bridgeStreamEvent{Kind: "start", ResponseID: id, Model: jsonutil.StringAt(value, "model")})
		}
	}
	if usageMap := jsonutil.MapAt(value, "usage"); len(usageMap) > 0 {
		usage := decodeOpenAIUsage(usageMap)
		events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
	}
	for _, raw := range jsonutil.SliceAt(value, "choices") {
		choice, _ := raw.(map[string]any)
		delta := jsonutil.MapAt(choice, "delta")
		// Stream fragments are payload: whitespace may separate words or form a whole delta.
		reasoning := jsonutil.StringAt(delta, "reasoning_content")
		if reasoning == "" {
			reasoning = jsonutil.StringAt(delta, "reasoning")
		}
		if reasoning != "" {
			events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: reasoning})
		}
		if text := jsonutil.StringAt(delta, "content"); text != "" {
			events = append(events, bridgeStreamEvent{Kind: "text", Text: text})
		}
		for _, rawCall := range jsonutil.SliceAt(delta, "tool_calls") {
			call, _ := rawCall.(map[string]any)
			key := fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id")))
			function := jsonutil.MapAt(call, "function")
			id := jsonutil.StringAt(call, "id")
			if _, seen := parser.tools[key]; !seen {
				parser.tools[key] = false
				parser.toolOrder = append(parser.toolOrder, key)
			}
			if id != "" {
				parser.toolIDs[key] = id
			}
			if name := jsonutil.StringAt(function, "name"); name != "" {
				parser.toolNames[key] = mergeToolName(parser.toolNames[key], name)
			}
			if arguments := jsonutil.StringAt(function, "arguments"); arguments != "" {
				if !parser.tools[key] && parser.toolNames[key] != "" {
					parser.tools[key] = true
					events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
				}
				events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: arguments})
			}
		}
		if stop := jsonutil.StringAt(choice, "finish_reason"); stop != "" {
			if isStreamErrorFinish(stop) {
				events = append(events, bridgeStreamEvent{Kind: "error", Error: jsonutil.FirstString(jsonutil.StringAt(choice, "error", "message"), "upstream Chat stream failed"), ErrorType: "upstream_error"})
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

// parseAnthropicEvent turns one Anthropic stream event into bridge events,
// keyed by its type (or the event name when the payload carries none):
// message_start supplies the ID, model and usage, content blocks yield
// reasoning, text and tool events, message_delta the usage and stop, and
// message_stop the done event. A stop reason that reports an error becomes an
// error event instead.
func (parser *bridgeStreamParser) parseAnthropicEvent(eventName string, value map[string]any) ([]bridgeStreamEvent, error) {
	typeName := jsonutil.StringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "message_start":
		message := jsonutil.MapAt(value, "message")
		events := []bridgeStreamEvent{{Kind: "start", ResponseID: jsonutil.StringAt(message, "id"), Model: jsonutil.StringAt(message, "model")}}
		if usageMap := jsonutil.MapAt(message, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return events, nil
	case "content_block_start":
		block := jsonutil.MapAt(value, "content_block")
		key := fmt.Sprint(value["index"])
		switch jsonutil.StringAt(block, "type") {
		case "thinking":
			events := make([]bridgeStreamEvent, 0, 2)
			if thinking := jsonutil.StringAt(block, "thinking"); thinking != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: thinking})
			}
			if signature := jsonutil.StringAt(block, "signature"); signature != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning_signature", Signature: signature})
			}
			return events, nil
		case "redacted_thinking":
			return []bridgeStreamEvent{{Kind: "reasoning", Encrypted: jsonutil.StringAt(block, "data")}}, nil
		case "tool_use":
			parser.tools[key] = true
			return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: jsonutil.StringAt(block, "id"), ToolName: jsonutil.StringAt(block, "name")}}, nil
		case "text":
			if text := jsonutil.StringAt(block, "text"); text != "" {
				return []bridgeStreamEvent{{Kind: "text", Text: text}}, nil
			}
		}
	case "content_block_delta":
		delta := jsonutil.MapAt(value, "delta")
		key := fmt.Sprint(value["index"])
		switch jsonutil.StringAt(delta, "type") {
		case "thinking_delta":
			return []bridgeStreamEvent{{Kind: "reasoning", Text: jsonutil.StringAt(delta, "thinking")}}, nil
		case "signature_delta":
			return []bridgeStreamEvent{{Kind: "reasoning_signature", Signature: jsonutil.StringAt(delta, "signature")}}, nil
		case "text_delta":
			return []bridgeStreamEvent{{Kind: "text", Text: jsonutil.StringAt(delta, "text")}}, nil
		case "input_json_delta":
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, Text: jsonutil.StringAt(delta, "partial_json")}}, nil
		}
	case "message_delta":
		events := make([]bridgeStreamEvent, 0, 2)
		if usageMap := jsonutil.MapAt(value, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		if stop := jsonutil.StringAt(value, "delta", "stop_reason"); stop != "" {
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
		message := jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), "upstream Anthropic stream error")
		return []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: jsonutil.FirstString(jsonutil.StringAt(value, "error", "type"), "upstream_error")}}, nil
	}
	return nil, nil
}

// parseResponses turns one Responses stream event into bridge events, keyed by
// its type (or the event name when the payload carries none): deltas become
// text, reasoning and tool deltas, item events open and close tool calls, and
// a terminal event reports the usage, the stop reason and done. A failed
// response becomes an error event after any usage it reports. A tool's
// arguments are taken from its deltas, and from the done event only when no
// delta arrived.
func (parser *bridgeStreamParser) parseResponses(eventName string, value map[string]any) []bridgeStreamEvent {
	typeName := jsonutil.StringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "error":
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Responses stream error"), ErrorType: streamErrorType(value)}}
	case "response.created":
		response := jsonutil.MapAt(value, "response")
		return []bridgeStreamEvent{{Kind: "start", ResponseID: jsonutil.StringAt(response, "id"), Model: jsonutil.StringAt(response, "model")}}
	case "response.output_text.delta":
		return []bridgeStreamEvent{{Kind: "text", Text: jsonutil.StringAt(value, "delta")}}
	case "response.reasoning_summary_text.delta":
		key := responseToolKey(value, nil)
		parser.responseReasoning[key] = true
		return []bridgeStreamEvent{{Kind: "reasoning", Text: jsonutil.StringAt(value, "delta")}}
	case "response.output_item.added":
		item := jsonutil.MapAt(value, "item")
		if jsonutil.StringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "name"))
			if parser.toolNames[key] != "" {
				parser.tools[key] = true
				return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]}}
			}
		}
	case "response.function_call_arguments.delta":
		key := responseToolKey(value, nil)
		parser.responseArgs[key] = true
		return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: jsonutil.StringAt(value, "delta")}}
	case "response.function_call_arguments.done":
		key := responseToolKey(value, nil)
		if !parser.responseArgs[key] {
			parser.responseArgs[key] = true
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: jsonutil.StringAt(value, "arguments")}}
		}
	case "response.output_item.done":
		item := jsonutil.MapAt(value, "item")
		if jsonutil.StringAt(item, "type") == "reasoning" {
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
		if jsonutil.StringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "name"))
			events := make([]bridgeStreamEvent, 0, 2)
			if !parser.tools[key] {
				parser.tools[key] = true
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
			}
			if !parser.responseArgs[key] {
				if arguments := jsonutil.StringAt(item, "arguments"); arguments != "" {
					events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, Text: arguments})
				}
			}
			return events
		}
	case "response.completed", "response.incomplete", "response.failed", "response.done":
		response := jsonutil.MapAt(value, "response")
		usageMap := jsonutil.MapAt(response, "usage")
		if typeName == "response.failed" || jsonutil.StringAt(response, "status") == "failed" {
			message := jsonutil.FirstString(jsonutil.StringAt(response, "error", "message"), jsonutil.StringAt(value, "error", "message"), "upstream Responses request failed")
			events := []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: jsonutil.FirstString(jsonutil.StringAt(response, "error", "code"), "upstream_error")}}
			if len(usageMap) > 0 {
				usage := decodeOpenAIUsage(usageMap)
				events = append([]bridgeStreamEvent{{Kind: "usage", Usage: &usage}}, events...)
			}
			return events
		}
		stop := "stop"
		if typeName == "response.incomplete" {
			stop = canonicalResponsesIncomplete(jsonutil.StringAt(response, "incomplete_details", "reason"))
		} else {
			stop = parser.responsesCompletedStop(response)
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

// responsesCompletedStop decides how a finished Responses turn is reported
// downstream. The completed event carries no stop reason, so the function_call
// items in its output — or the tool calls observed earlier on the stream — are
// the only signal that the turn ended waiting for tool results. Defaulting to
// "stop" made every tool step terminate the downstream turn instead of chaining
// inside it.
func (parser *bridgeStreamParser) responsesCompletedStop(response map[string]any) string {
	for _, raw := range jsonutil.SliceAt(response, "output") {
		item, _ := raw.(map[string]any)
		if jsonutil.StringAt(item, "type") == "function_call" {
			return "tool_calls"
		}
	}
	if len(parser.toolOrder) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// streamErrorMessage extracts the upstream error text, accepting the OpenAI
// nested error object, a top-level message and a bare error string.
func streamErrorMessage(value map[string]any, fallback string) string {
	if message := jsonutil.StringAt(value, "error", "message"); message != "" {
		return message
	}
	if message := jsonutil.StringAt(value, "message"); message != "" {
		return message
	}
	if message, ok := value["error"].(string); ok && message != "" {
		return message
	}
	return fallback
}

// streamErrorType extracts the upstream error type, falling back to its code
// and then to "upstream_error".
func streamErrorType(value map[string]any) string {
	return jsonutil.FirstString(jsonutil.StringAt(value, "error", "type"), jsonutil.StringAt(value, "error", "code"), "upstream_error")
}

// isStreamErrorFinish reports whether a finish or stop reason reports an
// upstream error rather than a normal end.
func isStreamErrorFinish(stop string) bool {
	switch strings.ToLower(strings.TrimSpace(stop)) {
	case "error", "network_error", "server_error":
		return true
	default:
		return false
	}
}

// rememberTool records a tool call seen in a Responses stream: its key is
// remembered as not yet started, and its ID and name are stored when given.
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

// mergeToolName folds a Chat name fragment into the name accumulated so far.
// A fragment that repeats the current name replaces it, one that extends it
// (the upstream re-sends a growing prefix) becomes the new name, and anything
// else is appended.
func mergeToolName(current, fragment string) string {
	if current == "" || fragment == current {
		return fragment
	}
	if strings.HasPrefix(fragment, current) {
		return fragment
	}
	return current + fragment
}

// responseToolKey identifies the tool call a Responses event belongs to: the
// item's id when the event carries an item, else the event's item_id, else its
// output_index.
func responseToolKey(event map[string]any, item map[string]any) string {
	if item != nil {
		if id := jsonutil.StringAt(item, "id"); id != "" {
			return id
		}
	}
	if id := jsonutil.StringAt(event, "item_id"); id != "" {
		return id
	}
	return fmt.Sprint(event["output_index"])
}
