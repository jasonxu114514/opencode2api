package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

// ConvertResponse converts a serialized response body from one protocol to
// another. A same-protocol body is returned as a copy, unchanged; otherwise
// the body is decoded into the bridge model and re-encoded for the target. An
// upstream error envelope becomes an error, and so does a body the target
// protocol cannot represent.
func ConvertResponse(from, to Protocol, body []byte) ([]byte, error) {
	if from == to {
		return append([]byte(nil), body...), nil
	}
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, err
	}
	response, err := decodeBridgeResponse(from, input)
	if err != nil {
		return nil, err
	}
	if response.Error != "" || response.Stop == "error" {
		return nil, fmt.Errorf("upstream response failed: %s", jsonutil.FirstString(response.Error, "upstream response failed"))
	}
	return json.Marshal(encodeBridgeResponse(to, response))
}

// decodeBridgeResponse decodes a complete response body of the given protocol
// into the bridge model. An error envelope or error status is reported in
// response.Error (and the caller turns it into an error), an unknown output
// item, content block or protocol is an error, and a missing creation time
// defaults to now.
func decodeBridgeResponse(protocol Protocol, input map[string]any) (bridgeResponse, error) {
	response := bridgeResponse{
		ID:      jsonutil.StringAt(input, "id"),
		Model:   jsonutil.StringAt(input, "model"),
		Created: jsonutil.Int64At(input, "created"),
	}
	if response.Created == 0 {
		response.Created = jsonutil.Int64At(input, "created_at")
	}
	if response.Created == 0 {
		response.Created = time.Now().Unix()
	}
	switch protocol {
	case Chat:
		if message := jsonutil.StringAt(input, "error", "message"); message != "" {
			response.Error = message
		}
		if response.Error != "" {
			return response, nil
		}
		choices := jsonutil.SliceAt(input, "choices")
		if len(choices) == 0 {
			return response, fmt.Errorf("chat response contains no choices")
		}
		choice, _ := choices[0].(map[string]any)
		message := jsonutil.MapAt(choice, "message")
		response.Reasoning = decodeChatReasoning(message)
		blocks, err := decodeOpenAIBlocksChecked(message["content"])
		if err != nil {
			return response, fmt.Errorf("chat response content: %w", err)
		}
		response.Text, err = responseTextBlocks(blocks, "Chat")
		if err != nil {
			return response, err
		}
		for _, raw := range jsonutil.SliceAt(message, "tool_calls") {
			call, _ := raw.(map[string]any)
			function := jsonutil.MapAt(call, "function")
			response.Tools = append(response.Tools, bridgeBlock{
				Kind:          "tool_call",
				ID:            jsonutil.StringAt(call, "id"),
				Name:          jsonutil.StringAt(function, "name"),
				ArgumentsJSON: jsonutil.StringAt(function, "arguments"),
			})
		}
		response.Stop = canonicalChatStop(jsonutil.StringAt(choice, "finish_reason"))
		response.Usage = decodeOpenAIUsage(jsonutil.MapAt(input, "usage"))
	case Responses:
		for _, raw := range jsonutil.SliceAt(input, "output") {
			item, _ := raw.(map[string]any)
			switch jsonutil.StringAt(item, "type") {
			case "reasoning":
				response.Reasoning = append(response.Reasoning, decodeResponsesReasoning(item)...)
			case "message":
				blocks, err := decodeOpenAIBlocksChecked(item["content"])
				if err != nil {
					return response, fmt.Errorf("Responses response content: %w", err)
				}
				text, err := responseTextBlocks(blocks, "Responses")
				if err != nil {
					return response, err
				}
				response.Text += text
			case "function_call":
				response.Tools = append(response.Tools, bridgeBlock{
					Kind:          "tool_call",
					ID:            jsonutil.FirstString(jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "id")),
					Name:          jsonutil.StringAt(item, "name"),
					ArgumentsJSON: jsonutil.StringAt(item, "arguments"),
				})
			default:
				return response, fmt.Errorf("Responses response has unsupported output item type %q", jsonutil.StringAt(item, "type"))
			}
		}
		response.Stop = "stop"
		if len(response.Tools) > 0 {
			response.Stop = "tool_calls"
		}
		if jsonutil.StringAt(input, "status") == "incomplete" {
			response.Stop = canonicalResponsesIncomplete(jsonutil.StringAt(input, "incomplete_details", "reason"))
		} else if jsonutil.StringAt(input, "status") == "failed" {
			response.Stop = "error"
			response.Error = jsonutil.FirstString(jsonutil.StringAt(input, "error", "message"), "upstream Responses request failed")
		}
		response.Usage = decodeOpenAIUsage(jsonutil.MapAt(input, "usage"))
	case Anthropic:
		if jsonutil.StringAt(input, "type") == "error" {
			response.Error = jsonutil.FirstString(jsonutil.StringAt(input, "error", "message"), "upstream Anthropic request failed")
		}
		blocks, err := decodeAnthropicBlocksChecked(input["content"])
		if err != nil {
			return response, fmt.Errorf("Anthropic response content: %w", err)
		}
		for _, block := range blocks {
			if block.Kind == "reasoning" {
				response.Reasoning = append(response.Reasoning, block)
			} else if block.Kind == "text" {
				response.Text += block.Text
			} else if block.Kind == "tool_call" {
				response.Tools = append(response.Tools, block)
			} else if block.Kind != "text" {
				return response, fmt.Errorf("Anthropic response contains unsupported %s content block", block.Kind)
			}
		}
		response.Stop = canonicalAnthropicStop(jsonutil.StringAt(input, "stop_reason"))
		response.Usage = decodeAnthropicUsage(jsonutil.MapAt(input, "usage"))
	default:
		return response, fmt.Errorf("unsupported response protocol %q", protocol)
	}
	return response, nil
}

// encodeBridgeResponse encodes a bridge response as the target protocol's
// body. A tool stop with no usable tool block is demoted to a plain stop first
// (the reason is spelled out at the guard below), and an ID is synthesized when
// the response has none.
func encodeBridgeResponse(protocol Protocol, response bridgeResponse) map[string]any {
	if response.ID == "" {
		response.ID = identity.RandomID("resp", 12)
	}
	if isToolStop(response.Stop) {
		response.Tools = usableToolBlocks(response.Tools)
		if len(response.Tools) == 0 {
			// The turn ended as a tool call upstream, but no usable tool block
			// was ever assembled (for example, the name was missing). A
			// tool_use stop reason with no usable tool blocks makes strict
			// clients end the turn running nothing and reporting no error.
			// Demote to a plain stop so the client treats it as text
			// end-of-turn and continues.
			response.Stop = "stop"
		}
	}
	switch protocol {
	case Chat:
		message := map[string]any{"role": "assistant", "content": response.Text}
		if value, ok := bridgeReasoningText(response.Reasoning); ok {
			message["reasoning_content"] = value
		}
		if signature := bridgeReasoningSignature(response.Reasoning); signature != "" {
			message["reasoning_signature"] = signature
		}
		if len(response.Tools) > 0 {
			calls := make([]any, 0, len(response.Tools))
			for _, tool := range response.Tools {
				calls = append(calls, map[string]any{
					"id":   tool.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tool.Name,
						"arguments": bridgeArgumentsJSON(tool),
					},
				})
			}
			message["tool_calls"] = calls
		}
		return map[string]any{
			"id":      asPrefix(response.ID, "chatcmpl"),
			"object":  "chat.completion",
			"created": response.Created,
			"model":   response.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": chatStop(response.Stop),
			}},
			"usage": openAIUsage(response.Usage),
		}
	case Responses:
		output := make([]any, 0, len(response.Reasoning)+len(response.Tools)+1)
		for _, reasoning := range response.Reasoning {
			output = append(output, encodeResponsesReasoning(reasoning, true))
		}
		if response.Text != "" {
			output = append(output, map[string]any{
				"id":     identity.RandomID("msg", 12),
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        response.Text,
					"annotations": []any{},
				}},
			})
		}
		for _, tool := range response.Tools {
			output = append(output, map[string]any{
				"id":        identity.RandomID("fc", 12),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tool.ID,
				"name":      tool.Name,
				"arguments": bridgeArgumentsJSON(tool),
			})
		}
		status := "completed"
		var incomplete any
		if response.Stop == "length" || response.Stop == "content_filter" {
			status = "incomplete"
			incomplete = map[string]any{"reason": responsesIncompleteReason(response.Stop)}
		} else if response.Stop == "error" {
			status = "failed"
		}
		return map[string]any{
			"id":                 asPrefix(response.ID, "resp"),
			"object":             "response",
			"created_at":         response.Created,
			"status":             status,
			"model":              response.Model,
			"output":             output,
			"error":              nil,
			"incomplete_details": incomplete,
			"usage": map[string]any{
				"input_tokens":  response.Usage.Input,
				"output_tokens": response.Usage.Output,
				"total_tokens":  response.Usage.Total,
				"input_tokens_details": map[string]any{
					"cached_tokens": response.Usage.Cached,
				},
				"output_tokens_details": map[string]any{
					"reasoning_tokens": response.Usage.Reasoning,
				},
			},
		}
	case Anthropic:
		content := make([]any, 0, len(response.Reasoning)+len(response.Tools)+1)
		content = append(content, encodeAnthropicBlocks(response.Reasoning)...)
		if response.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": response.Text})
		}
		for _, tool := range response.Tools {
			input := tool.Arguments
			if input == nil {
				input = jsonutil.ParseJSONOrString(tool.ArgumentsJSON)
			}
			content = append(content, map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": input})
		}
		return map[string]any{
			"id":            asPrefix(response.ID, "msg"),
			"type":          "message",
			"role":          "assistant",
			"model":         response.Model,
			"content":       content,
			"stop_reason":   anthropicStop(response.Stop),
			"stop_sequence": nil,
			"usage":         anthropicUsage(response.Usage),
		}
	default:
		return map[string]any{}
	}
}

// decodeOpenAIUsage reads a Chat or Responses usage object. Either spelling of
// each field is accepted, prompt/input and completion/output alike, and the
// cache-read, cache-write and reasoning details are read from both the Chat
// and the Responses nesting.
func decodeOpenAIUsage(usage map[string]any) Usage {
	input := jsonutil.FirstNonZero(jsonutil.IntAt(usage, "prompt_tokens"), jsonutil.IntAt(usage, "input_tokens"))
	output := jsonutil.FirstNonZero(jsonutil.IntAt(usage, "completion_tokens"), jsonutil.IntAt(usage, "output_tokens"))
	total := jsonutil.IntAt(usage, "total_tokens")
	if total == 0 {
		total = input + output
	}
	cached := jsonutil.FirstNonZero(jsonutil.IntAt(usage, "prompt_tokens_details", "cached_tokens"), jsonutil.IntAt(usage, "input_tokens_details", "cached_tokens"))
	cacheCreation := jsonutil.FirstNonZero(
		jsonutil.IntAt(usage, "cache_creation_input_tokens"),
		jsonutil.IntAt(usage, "prompt_tokens_details", "cache_creation_input_tokens"),
		jsonutil.IntAt(usage, "prompt_tokens_details", "cache_write_tokens"),
		jsonutil.IntAt(usage, "input_tokens_details", "cache_creation_input_tokens"),
	)
	reasoning := jsonutil.FirstNonZero(jsonutil.IntAt(usage, "completion_tokens_details", "reasoning_tokens"), jsonutil.IntAt(usage, "output_tokens_details", "reasoning_tokens"))
	return Usage{Input: input, Output: output, Total: total, Cached: cached, CacheCreation: cacheCreation, Reasoning: reasoning}
}

// decodeAnthropicUsage reads an Anthropic usage object. Anthropic reports the
// cache parts separately from input_tokens, so Input is the sum of the three,
// matching the prompt-token semantics the bridge uses.
func decodeAnthropicUsage(usage map[string]any) Usage {
	input := jsonutil.IntAt(usage, "input_tokens")
	cached := jsonutil.IntAt(usage, "cache_read_input_tokens")
	cacheCreation := jsonutil.IntAt(usage, "cache_creation_input_tokens")
	output := jsonutil.IntAt(usage, "output_tokens")
	input += cached + cacheCreation
	return Usage{
		Input:         input,
		Output:        output,
		Total:         input + output,
		Cached:        cached,
		CacheCreation: cacheCreation,
	}
}

// openAIUsage renders bridge usage in the Chat usage shape.
func openAIUsage(usage Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     usage.Input,
		"completion_tokens": usage.Output,
		"total_tokens":      usage.Total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.Cached,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.Reasoning,
		},
	}
}

// anthropicUsage renders bridge usage in the Anthropic shape, where
// input_tokens excludes the cache parts, so it is Input minus Cached and
// CacheCreation, floored at zero.
func anthropicUsage(usage Usage) map[string]any {
	return map[string]any{
		"input_tokens":                max(usage.Input-usage.Cached-usage.CacheCreation, 0),
		"output_tokens":               usage.Output,
		"cache_creation_input_tokens": usage.CacheCreation,
		"cache_read_input_tokens":     usage.Cached,
	}
}

// chatStop maps a canonical bridge stop reason onto Chat's finish_reason.
func chatStop(stop string) string {
	switch stop {
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "length":
		return "length"
	case "content_filter", "error":
		return stop
	default:
		return "stop"
	}
}

// anthropicStop maps a canonical bridge stop reason onto Anthropic's
// stop_reason.
func anthropicStop(stop string) string {
	switch stop {
	case "tool_use", "tool_calls":
		return "tool_use"
	case "max_tokens", "length":
		return "max_tokens"
	case "stop_sequence", "pause_turn", "refusal":
		return stop
	default:
		return "end_turn"
	}
}

// canonicalChatStop maps Chat's finish_reason onto a canonical bridge stop
// reason.
func canonicalChatStop(stop string) string {
	switch stop {
	case "tool_calls", "function_call":
		return "tool_calls"
	case "length":
		return "length"
	case "content_filter":
		return "content_filter"
	case "error", "network_error", "server_error":
		return "error"
	default:
		return "stop"
	}
}

// canonicalAnthropicStop maps Anthropic's stop_reason onto a canonical bridge
// stop reason.
func canonicalAnthropicStop(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "stop_sequence", "pause_turn", "refusal":
		return stop
	case "error", "network_error", "server_error":
		return "error"
	default:
		return "stop"
	}
}

// isToolStop reports whether a stop reason promises tool calls downstream. It
// accepts the canonical "tool_calls" as well as the raw "tool_use" and
// "function_call" spellings an upstream may report.
func isToolStop(stop string) bool {
	switch stop {
	case "tool_calls", "tool_use", "function_call":
		return true
	default:
		return false
	}
}

// usableToolBlocks drops phantom tool blocks that cannot be executed by a
// downstream client. Empty arguments are valid, so the tool name is the
// minimum required signal here: a name that is empty or only whitespace is
// dropped. The streaming emitter applies a looser version of the rule, opening
// a tool as soon as its name is non-empty.
func usableToolBlocks(tools []bridgeBlock) []bridgeBlock {
	if len(tools) == 0 {
		return nil
	}
	usable := tools[:0]
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		usable = append(usable, tool)
	}
	return usable
}

// canonicalResponsesIncomplete maps a Responses incomplete_details.reason onto
// a canonical bridge stop reason: "content_filter" stays itself and any other
// reason, max_output_tokens included, becomes "length".
func canonicalResponsesIncomplete(reason string) string {
	switch reason {
	case "content_filter":
		return "content_filter"
	default:
		return "length"
	}
}

// responsesIncompleteReason is the inverse of canonicalResponsesIncomplete:
// the incomplete_details.reason to report for a canonical stop.
func responsesIncompleteReason(stop string) string {
	if stop == "content_filter" {
		return "content_filter"
	}
	return "max_output_tokens"
}

// responseTextBlocks joins the text of blocks, erroring on any other kind:
// neither a Chat message nor a Responses output message may carry more than
// text.
func responseTextBlocks(blocks []bridgeBlock, protocol string) (string, error) {
	for _, block := range blocks {
		if block.Kind != "text" {
			return "", fmt.Errorf("%s response contains unsupported %s content block", protocol, block.Kind)
		}
	}
	return bridgeBlocksText(blocks), nil
}

// ResponseUsage extracts the usage of a serialized response body and reports
// whether it carried any. The body is decoded per protocol, so an Anthropic
// usage is read with its separate cache fields.
func ResponseUsage(protocol Protocol, body []byte) (Usage, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return Usage{}, false
	}
	usage := jsonutil.MapAt(payload, "usage")
	if len(usage) == 0 {
		return Usage{}, false
	}
	if protocol == Anthropic {
		return decodeAnthropicUsage(usage), true
	}
	return decodeOpenAIUsage(usage), true
}

// mayCarryToolName is the fast path for a response body or SSE frame: a body
// that mentions neither a client alias nor a tool-call marker cannot name a
// tool, so it is handed on without decoding. The markers are the protocol's
// own spellings; folding an unprefixed official name (todowrite) needs the
// frame decoded even when no clint_ alias is present.
func mayCarryToolName(body []byte) bool {
	return bytes.Contains(body, []byte(ClientToolPrefix)) ||
		bytes.Contains(body, []byte(`"tool_calls"`)) ||
		bytes.Contains(body, []byte(`"function_call"`)) ||
		bytes.Contains(body, []byte(`"custom_tool_call"`)) ||
		bytes.Contains(body, []byte(`"tool_use"`)) ||
		bytes.Contains(body, []byte(`"server_tool_use"`))
}

// RenameResponseTool rewrites tool call names that carry a client alias back to
// the name the client declared, inside a serialized JSON response or SSE event
// data object for the given protocol. It is the counterpart of the outbound
// clint_ renaming applied to shaped free-tier requests.
//
// mayCarryToolName is only a fast path that skips the unmarshal for frames that
// cannot contain a tool name; every rewrite is a map lookup after decoding.
// Names that map to nothing are left alone.
func RenameResponseTool(body []byte, protocol Protocol, renames *ToolRenames) []byte {
	if renames.Empty() || len(body) == 0 {
		return body
	}
	if !mayCarryToolName(body) {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if !renameToolInMap(payload, protocol, renames) {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// renameToolInMap rewrites aliased tool names in a decoded response or stream
// event and reports whether anything changed. Every place a protocol reports a
// tool call is visited: Chat message, delta and legacy function_call tool
// calls, Responses function_call and custom_tool_call items under "item",
// "output" and a "response" envelope, and Anthropic tool_use blocks under
// "content_block" or "content".
func renameToolInMap(payload map[string]any, protocol Protocol, renames *ToolRenames) bool {
	rename := func(container map[string]any) bool {
		name, ok := container["name"].(string)
		if !ok {
			return false
		}
		client, changed := renames.Resolve(name)
		if !changed {
			return false
		}
		container["name"] = client
		return true
	}
	renameItem := func(item map[string]any) bool {
		switch jsonutil.StringAt(item, "type") {
		case "function_call", "custom_tool_call":
			return rename(item)
		default:
			return false
		}
	}
	changed := false
	switch protocol {
	case Chat:
		for _, rawChoice := range jsonutil.SliceAt(payload, "choices") {
			choice, _ := rawChoice.(map[string]any)
			for _, key := range []string{"message", "delta"} {
				container := jsonutil.MapAt(choice, key)
				for _, rawCall := range jsonutil.SliceAt(container, "tool_calls") {
					call, _ := rawCall.(map[string]any)
					if function, ok := call["function"].(map[string]any); ok && rename(function) {
						changed = true
					}
				}
				if function, ok := container["function_call"].(map[string]any); ok && rename(function) {
					changed = true
				}
			}
		}
	case Responses:
		if item, ok := payload["item"].(map[string]any); ok && renameItem(item) {
			changed = true
		}
		for _, rawItem := range jsonutil.SliceAt(payload, "output") {
			item, _ := rawItem.(map[string]any)
			if renameItem(item) {
				changed = true
			}
		}
		if resp, ok := payload["response"].(map[string]any); ok {
			for _, rawItem := range jsonutil.SliceAt(resp, "output") {
				item, _ := rawItem.(map[string]any)
				if renameItem(item) {
					changed = true
				}
			}
		}
	case Anthropic:
		if block, ok := payload["content_block"].(map[string]any); ok {
			if jsonutil.StringAt(block, "type") == "tool_use" && rename(block) {
				changed = true
			}
		}
		for _, rawBlock := range jsonutil.SliceAt(payload, "content") {
			block, _ := rawBlock.(map[string]any)
			if jsonutil.StringAt(block, "type") == "tool_use" && rename(block) {
				changed = true
			}
		}
	}
	return changed
}
