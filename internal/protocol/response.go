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

func anthropicUsage(usage Usage) map[string]any {
	return map[string]any{
		"input_tokens":                max(usage.Input-usage.Cached-usage.CacheCreation, 0),
		"output_tokens":               usage.Output,
		"cache_creation_input_tokens": usage.CacheCreation,
		"cache_read_input_tokens":     usage.Cached,
	}
}

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

// isToolStop reports whether a canonical bridge stop reason promises tool
// calls downstream.
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
// minimum required signal here; the streaming emitter uses the same rule.
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

func canonicalResponsesIncomplete(reason string) string {
	switch reason {
	case "content_filter":
		return "content_filter"
	default:
		return "length"
	}
}

func responsesIncompleteReason(stop string) string {
	if stop == "content_filter" {
		return "content_filter"
	}
	return "max_output_tokens"
}

func responseTextBlocks(blocks []bridgeBlock, protocol string) (string, error) {
	for _, block := range blocks {
		if block.Kind != "text" {
			return "", fmt.Errorf("%s response contains unsupported %s content block", protocol, block.Kind)
		}
	}
	return bridgeBlocksText(blocks), nil
}

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

// RenameResponseTool rewrites tool call names matching fromName to toName inside
// a serialized JSON response or SSE event data object for the given protocol.
// It is used to restore a client's original read-related tool name (e.g.
// "reAd" or "read_file") after the legacy /zen/v1 shaper aliased it to "read"
// on the outbound wire.
//
// fromName is guaranteed not to occur as a substring of any other text in a
// response (tool names are never echoed back inside content), so a byte-level
// occurrence check is enough to skip the re-marshal for the vast majority of
// frames.
func RenameResponseTool(body []byte, protocol Protocol, fromName, toName string) []byte {
	if fromName == "" || toName == "" || fromName == toName || len(body) == 0 {
		return body
	}
	if !bytes.Contains(body, []byte(fromName)) {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if !renameToolInMap(payload, protocol, fromName, toName) {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func renameToolInMap(payload map[string]any, protocol Protocol, fromName, toName string) bool {
	changed := false
	switch protocol {
	case Chat:
		for _, rawChoice := range jsonutil.SliceAt(payload, "choices") {
			choice, _ := rawChoice.(map[string]any)
			for _, key := range []string{"message", "delta"} {
				container := jsonutil.MapAt(choice, key)
				for _, rawCall := range jsonutil.SliceAt(container, "tool_calls") {
					call, _ := rawCall.(map[string]any)
					if function, ok := call["function"].(map[string]any); ok && jsonutil.StringAt(function, "name") == fromName {
						function["name"] = toName
						changed = true
					}
				}
			}
		}
	case Responses:
		if item, ok := payload["item"].(map[string]any); ok {
			if jsonutil.StringAt(item, "type") == "function_call" && jsonutil.StringAt(item, "name") == fromName {
				item["name"] = toName
				changed = true
			}
		}
		for _, rawItem := range jsonutil.SliceAt(payload, "output") {
			item, _ := rawItem.(map[string]any)
			if jsonutil.StringAt(item, "type") == "function_call" && jsonutil.StringAt(item, "name") == fromName {
				item["name"] = toName
				changed = true
			}
		}
		if resp, ok := payload["response"].(map[string]any); ok {
			for _, rawItem := range jsonutil.SliceAt(resp, "output") {
				item, _ := rawItem.(map[string]any)
				if jsonutil.StringAt(item, "type") == "function_call" && jsonutil.StringAt(item, "name") == fromName {
					item["name"] = toName
					changed = true
				}
			}
		}
	case Anthropic:
		if block, ok := payload["content_block"].(map[string]any); ok {
			if jsonutil.StringAt(block, "type") == "tool_use" && jsonutil.StringAt(block, "name") == fromName {
				block["name"] = toName
				changed = true
			}
		}
		for _, rawBlock := range jsonutil.SliceAt(payload, "content") {
			block, _ := rawBlock.(map[string]any)
			if jsonutil.StringAt(block, "type") == "tool_use" && jsonutil.StringAt(block, "name") == fromName {
				block["name"] = toName
				changed = true
			}
		}
	}
	return changed
}
