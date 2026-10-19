package protocol

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"opencode2api/internal/jsonutil"
)

// PrepareRequest is the single request preparation path for both
// pass-through and transcoded requests. Same-protocol requests are cloned so
// provider-specific fields survive, while cross-protocol requests go through
// the bridge. Target-protocol normalization then repairs reasoning history in
// either case, using the model and upstreamURL to recognize reasoning
// vendors, and a Responses body has its max_output_tokens floored.
func PrepareRequest(from, to Protocol, input map[string]any, upstreamURL string) (map[string]any, error) {
	output, err := ConvertRequest(from, to, input)
	if err != nil {
		return nil, err
	}
	if to == Responses {
		// The bridge already floors converted bodies; a same-protocol body is
		// a clone of the client's and needs the same guard.
		jsonutil.Put(output, "max_output_tokens", floorResponsesMaxTokens(output["max_output_tokens"]))
	}
	normalizeToolReasoningHistory(to, jsonutil.StringAt(output, "model"), upstreamURL, output)
	return output, nil
}

// ForcedEffort applies an operator-configured thinking level to a prepared
// upstream body, so a client can be served at a fixed level without the client
// cooperating. A level validForcedEffort rejects is ignored, and "none"
// removes the reasoning configuration instead of setting a level.
//
// A level already present in the body always wins (see clientEffortExplicit).
// The forced level fills in only where the body carries no level in the slot
// that protocol's encoder writes, such as a request that said nothing about
// reasoning, or a same-protocol Anthropic request whose thinking block names
// only a budget; that budget is then rewritten to the forced level's. A body
// the bridge converted has that slot filled whenever it derived a level from
// thinking.budget_tokens, so the derived level counts as present and is kept.
// The level is applied to the finished upstream body rather than to the bridge
// value because a same-protocol request is cloned and never passes through the
// bridge; the body is the one place both paths share.
func ForcedEffort(protocol Protocol, body map[string]any, effort string) {
	if body == nil {
		return
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if !validForcedEffort(effort) {
		return
	}
	disable := effort == "none"
	switch protocol {
	case Chat:
		if clientEffortExplicit(protocol, body) {
			return
		}
		if disable {
			delete(body, "reasoning_effort")
			return
		}
		body["reasoning_effort"] = effort
	case Anthropic:
		if clientEffortExplicit(protocol, body) {
			return
		}
		applyAnthropicForcedEffort(body, effort, disable)
	case Responses:
		if clientEffortExplicit(protocol, body) {
			return
		}
		if disable {
			delete(body, "reasoning")
			return
		}
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
			body["reasoning"] = reasoning
		}
		reasoning["effort"] = effort
	}
}

// clientEffortExplicit reports whether an upstream body already carries a
// level in its protocol's native slot, which a forced level must not replace.
//
// The shapes differ per protocol, and each one mirrors how that protocol
// expresses the level natively: Anthropic in a non-blank output_config.effort
// (or top-level effort) string, Chat in a non-blank reasoning_effort string,
// string value counts for Responses, so it cannot tell a non-blank level
// the client stated from one derived from a budget. Blank values count as
// unset on every protocol; a missing or non-string value also counts as no
// level.
func clientEffortExplicit(protocol Protocol, body map[string]any) bool {
	switch protocol {
	case Chat:
		effort, ok := body["reasoning_effort"].(string)
		return ok && strings.TrimSpace(effort) != ""
	case Anthropic:
		if effort, ok := jsonutil.AnyAt(body, "output_config", "effort").(string); ok && strings.TrimSpace(effort) != "" {
			return true
		}
		effort, ok := body["effort"].(string)
		return ok && strings.TrimSpace(effort) != ""
	case Responses:
		effort, ok := jsonutil.AnyAt(body, "reasoning", "effort").(string)
		return ok && strings.TrimSpace(effort) != ""
	default:
		return false
	}
}

// applyAnthropicForcedEffort writes a forced level into an Anthropic body and
// keeps the thinking block and max_tokens consistent with it.
//
// Anthropic represents a level as output_config.effort, but also requires a
// thinking block to carry a budget and requires max_tokens to exceed that
// budget. Forcing a level therefore has to move all three together, or the
// upstream rejects the request it was just told to run at a higher level. With
// disable set, the thinking block and output_config are removed instead.
func applyAnthropicForcedEffort(body map[string]any, effort string, disable bool) {
	if disable {
		delete(body, "thinking")
		delete(body, "output_config")
		return
	}
	if outputConfig, _ := body["output_config"].(map[string]any); outputConfig == nil {
		body["output_config"] = map[string]any{"effort": effort}
	} else {
		outputConfig["effort"] = effort
	}
	budget := budgetForEffort(effort)
	if thinking, ok := body["thinking"].(map[string]any); ok {
		thinking["type"] = "enabled"
		thinking["budget_tokens"] = float64(budget)
	} else {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": float64(budget)}
	}
	// max_tokens must stay strictly greater than the thinking budget, otherwise
	// Anthropic rejects the payload regardless of the requested effort.
	if current := jsonutil.IntAt(body, "max_tokens"); current > 0 && current <= budget {
		body["max_tokens"] = budget + 4096
	}
}

// validForcedEffort accepts the levels the configuration can force. "none" is
// the escape hatch that removes reasoning again.
func validForcedEffort(effort string) bool {
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max", "none":
		return true
	default:
		return false
	}
}

// normalizeToolReasoningHistory repairs the reasoning history of assistant
// tool-call turns in a prepared upstream body and reports whether it changed
// anything. It applies only to endpoints that are known to require reasoning
// replay, identified by the model or upstream URL naming a reasoning vendor;
// a Chat body is also repaired when the request explicitly enables reasoning.
// Responses bodies are never touched. Normalizing the target shape makes the
// behavior independent of the client protocol used to reach the gateway.
func normalizeToolReasoningHistory(protocol Protocol, model, upstreamURL string, input map[string]any) bool {
	vendor := isReasoningVendorIdentifier(model) || isReasoningVendorIdentifier(upstreamURL)
	widened := vendor || requestEnablesReasoning(input)
	switch protocol {
	case Chat:
		// The Chat repair is additive: it only supplies reasoning_content that
		// a compatible endpoint ignores when it does not understand the field.
		// The widened gate is therefore safe here.
		if !widened {
			return false
		}
		return normalizeChatToolReasoningHistory(input)
	case Anthropic:
		// Every repair below is destructive for a native Anthropic endpoint:
		// it deletes thinking signatures, discards redacted_thinking payloads
		// and overwrites empty thinking text. Those are precisely the shapes a
		// native endpoint re-validates and replays verbatim, and rewriting them
		// also shifts the cached prompt prefix mid-history. Only endpoints
		// identified as reasoning vendors get the rewrite.
		if !vendor {
			return false
		}
		return normalizeAnthropicToolThinkingHistory(input)
	default:
		return false
	}
}

// isReasoningVendorIdentifier reports whether a model ID or upstream URL
// contains one of the reasoningVendorHints, ignoring case.
func isReasoningVendorIdentifier(value string) bool {
	value = strings.ToLower(value)
	for _, hint := range reasoningVendorHints {
		if strings.Contains(value, hint) {
			return true
		}
	}
	return false
}

// requestEnablesReasoning reports whether a body's reasoning_effort,
// reasoning, thinking or effort field asks for reasoning. A string counts
// unless it is blank, "none" or "disabled"; a bool counts when true; an object
// counts unless its type (or, without one, its effort) is "none" or
// "disabled"; any other non-null value counts.
func requestEnablesReasoning(input map[string]any) bool {
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "effort"} {
		value, exists := input[key]
		if !exists || value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			mode := strings.ToLower(strings.TrimSpace(typed))
			if mode != "" && mode != "none" && mode != "disabled" {
				return true
			}
		case bool:
			if typed {
				return true
			}
		case map[string]any:
			mode := strings.ToLower(strings.TrimSpace(jsonutil.FirstString(jsonutil.StringAt(typed, "type"), jsonutil.StringAt(typed, "effort"))))
			if mode == "none" || mode == "disabled" {
				continue
			}
			return true
		default:
			return true
		}
	}
	return false
}

// normalizeChatToolReasoningHistory ensures every assistant tool-call turn
// carries reasoning_content. Some clients discard this non-standard field
// while retaining tool_calls, which otherwise makes the next thinking-mode
// request invalid. A legacy reasoning string is promoted when available, and a
// placeholder is used otherwise. It reports whether any message changed.
func normalizeChatToolReasoningHistory(input map[string]any) bool {
	messages, ok := input["messages"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || jsonutil.StringAt(message, "role") != "assistant" || len(jsonutil.SliceAt(message, "tool_calls")) == 0 {
			continue
		}
		if reasoning, ok := message["reasoning_content"].(string); ok && strings.TrimSpace(reasoning) != "" {
			continue
		}
		reasoning, _ := message["reasoning"].(string)
		if strings.TrimSpace(reasoning) == "" {
			reasoning = toolReasoningPlaceholder
		}
		message["reasoning_content"] = reasoning
		changed = true
	}
	return changed
}

// normalizeAnthropicToolThinkingHistory repairs only assistant turns that
// contain tool_use. DeepSeek, Kimi/Moonshot and MiMo reject signed, redacted,
// empty or missing thinking history on those turns even though Anthropic
// clients can legitimately send each of those shapes. It strips thinking
// signatures, replaces redacted_thinking blocks and empty thinking text with
// placeholders, and prepends a placeholder thinking block to a turn that has
// none. It reports whether any message changed.
func normalizeAnthropicToolThinkingHistory(input map[string]any) bool {
	messages, ok := input["messages"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || jsonutil.StringAt(message, "role") != "assistant" {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok || !anthropicContentHasType(content, "tool_use") {
			continue
		}

		hasThinking := false
		for i, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch jsonutil.StringAt(block, "type") {
			case "thinking":
				hasThinking = true
				if _, exists := block["signature"]; exists {
					delete(block, "signature")
					changed = true
				}
				thinking, ok := block["thinking"].(string)
				if !ok || strings.TrimSpace(thinking) == "" {
					block["thinking"] = toolReasoningPlaceholder
					changed = true
				}
			case "redacted_thinking":
				hasThinking = true
				content[i] = map[string]any{
					"type":     "thinking",
					"thinking": anthropicRedactedThinkingPlaceholder,
				}
				changed = true
			}
		}
		if !hasThinking {
			content = append([]any{map[string]any{
				"type":     "thinking",
				"thinking": toolReasoningPlaceholder,
			}}, content...)
			message["content"] = content
			changed = true
		}
	}
	return changed
}

// anthropicContentHasType reports whether any block of an Anthropic content
// array has the given type.
func anthropicContentHasType(content []any, kind string) bool {
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		if jsonutil.StringAt(block, "type") == kind {
			return true
		}
	}
	return false
}

// anthropicReasoning decodes the reasoning controls of an Anthropic Messages
// request into a single bridge-level value.
//
// An effort the client states explicitly outranks a thinking block's budget.
// Choosing input["thinking"] first (as this used to) shadowed
// output_config.effort and a top-level effort entirely, so `thinking:
// {adaptive}` plus `output_config: {effort: "max"}` decoded as the thinking
// block and was reported downstream as the budget bucket "high". A stated
// "none"/"disabled" decodes as no reasoning at all, even next to a thinking
// block.
func anthropicReasoning(input map[string]any) any {
	stated := jsonutil.FirstAny(jsonutil.AnyAt(input, "output_config", "effort"), input["effort"])
	if effort, ok := normalizeEffortValue(stated); ok {
		// The explicit effort survives next to the thinking block's budget so
		// nothing the client said is lost on the way to the bridge.
		if effort == nil {
			return nil
		}
		if thinking, ok := input["thinking"].(map[string]any); ok {
			merged := make(map[string]any, len(thinking)+1)
			for key, value := range thinking {
				merged[key] = value
			}
			merged["effort"] = effort
			return merged
		}
		return effort
	}
	return input["thinking"]
}

// decodeBridgeRequest decodes a client request body of the given protocol into
// the bridge model. It rejects what the bridge cannot carry faithfully: a
// malformed message or tool, an unsupported role, content block or Responses
// item type, and a Chat tool that is not a function. Built-in Responses tools
// are skipped, as is a Responses message item with a role other than system,
// developer, user or assistant. Tool results of Responses and Anthropic
// requests then go through decodeToolImages, and a result carrying images
// must name its tool call.
func decodeBridgeRequest(protocol Protocol, input map[string]any) (bridgeRequest, error) {
	request := bridgeRequest{
		Model:       jsonutil.StringAt(input, "model"),
		Stream:      jsonutil.BoolAt(input, "stream"),
		Temperature: input["temperature"],
		TopP:        input["top_p"],
		Metadata:    input["metadata"],
	}
	switch protocol {
	case Chat:
		request.MaxTokens = jsonutil.FirstAny(input["max_completion_tokens"], input["max_tokens"])
		request.Stop = input["stop"]
		request.Reasoning = jsonutil.FirstAny(input["reasoning_effort"], input["reasoning"])
		for i, raw := range jsonutil.SliceAt(input, "messages") {
			message, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("messages[%d] must be an object", i)
			}
			role := jsonutil.StringAt(message, "role")
			blocks, err := decodeOpenAIBlocksChecked(message["content"])
			if err != nil {
				return request, fmt.Errorf("messages[%d]: %w", i, err)
			}
			if role == "assistant" {
				blocks = append(decodeChatReasoning(message), blocks...)
				// A budget the gateway carried out on the assistant message is
				// read back next to the effort, so both directions agree on the
				// budget the client originally named.
				if budget := jsonutil.IntAt(message, "reasoning_budget_tokens"); budget > 0 {
					request.Reasoning = withReasoningBudget(request.Reasoning, budget)
				}
			}
			for j, rawCall := range jsonutil.SliceAt(message, "tool_calls") {
				call, ok := rawCall.(map[string]any)
				if !ok {
					return request, fmt.Errorf("messages[%d].tool_calls[%d] must be an object", i, j)
				}
				function := jsonutil.MapAt(call, "function")
				blocks = append(blocks, bridgeBlock{
					Kind:          "tool_call",
					ID:            jsonutil.StringAt(call, "id"),
					Name:          jsonutil.StringAt(function, "name"),
					ArgumentsJSON: jsonutil.StringAt(function, "arguments"),
				})
			}
			switch role {
			case "system":
				request.System = append(request.System, blocks...)
			case "developer":
				request.Developer = append(request.Developer, blocks...)
			case "tool":
				request.Messages = append(request.Messages, bridgeMessage{Role: "user", Blocks: []bridgeBlock{{
					Kind:   "tool_result",
					CallID: jsonutil.StringAt(message, "tool_call_id"),
					Result: message["content"],
				}}})
			case "user", "assistant":
				request.Messages = append(request.Messages, bridgeMessage{Role: role, Blocks: blocks})
			default:
				return request, fmt.Errorf("messages[%d] has unsupported role %q", i, role)
			}
		}
		for i, raw := range jsonutil.SliceAt(input, "tools") {
			tool, ok := raw.(map[string]any)
			if !ok || jsonutil.StringAt(tool, "type") != "function" {
				return request, fmt.Errorf("tools[%d] must be a function tool", i)
			}
			function := jsonutil.MapAt(tool, "function")
			request.Tools = append(request.Tools, bridgeTool{
				Name:        jsonutil.StringAt(function, "name"),
				Description: jsonutil.StringAt(function, "description"),
				Schema:      function["parameters"],
				Strict:      jsonutil.BoolAt(function, "strict"),
			})
		}
		request.ToolChoice = decodeChatToolChoice(input["tool_choice"])
		request.ResponseFormat = input["response_format"]
		request.ParallelToolCalls = input["parallel_tool_calls"]
		request.FrequencyPenalty = input["frequency_penalty"]
		request.PresencePenalty = input["presence_penalty"]
		request.Seed = input["seed"]
		request.PromptCacheKey = input["prompt_cache_key"]
		request.SafetyIdentifier = input["safety_identifier"]
		request.ServiceTier = input["service_tier"]
		request.Store = input["store"]

	case Responses:
		request.MaxTokens = input["max_output_tokens"]
		request.Stop = input["stop"]
		request.Reasoning = jsonutil.FirstAny(input["reasoning"], input["reasoning_effort"])
		instructions, err := decodeOpenAIBlocksChecked(input["instructions"])
		if err != nil {
			return request, fmt.Errorf("instructions: %w", err)
		}
		request.System = append(request.System, instructions...)
		switch value := input["input"].(type) {
		case string:
			request.Messages = append(request.Messages, bridgeMessage{Role: "user", Blocks: []bridgeBlock{{Kind: "text", Text: value}}})
		case []any:
			for i, raw := range value {
				item, ok := raw.(map[string]any)
				if !ok {
					return request, fmt.Errorf("input[%d] must be an object", i)
				}
				switch jsonutil.StringAt(item, "type") {
				case "reasoning":
					for _, block := range decodeResponsesReasoning(item) {
						appendBridgeBlock(&request.Messages, "assistant", block)
					}
				case "function_call":
					appendBridgeBlock(&request.Messages, "assistant", bridgeBlock{
						Kind:          "tool_call",
						ID:            jsonutil.FirstString(jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "id")),
						Name:          jsonutil.StringAt(item, "name"),
						ArgumentsJSON: jsonutil.StringAt(item, "arguments"),
					})
				case "function_call_output":
					appendBridgeBlock(&request.Messages, "user", bridgeBlock{
						Kind:   "tool_result",
						CallID: jsonutil.StringAt(item, "call_id"),
						Result: item["output"],
					})
				case "message", "":
					role := jsonutil.StringAt(item, "role")
					blocks, err := decodeOpenAIBlocksChecked(item["content"])
					if err != nil {
						return request, fmt.Errorf("input[%d]: %w", i, err)
					}
					if role == "system" {
						request.System = append(request.System, blocks...)
					} else if role == "developer" {
						request.Developer = append(request.Developer, blocks...)
					} else if role == "user" || role == "assistant" {
						request.Messages = append(request.Messages, bridgeMessage{Role: role, Blocks: blocks})
					}
				default:
					return request, fmt.Errorf("input[%d] has unsupported Responses item type %q", i, jsonutil.StringAt(item, "type"))
				}
			}
		default:
			return request, fmt.Errorf("input must be a string or array")
		}
		for i, raw := range jsonutil.SliceAt(input, "tools") {
			tool, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("tools[%d] must be an object", i)
			}
			// Built-in Responses tools (web_search, file_search, code_interpreter, …)
			// have no Chat/Anthropic equivalent; skip them during cross-protocol
			// bridging so requests that mix function tools with built-in plugins
			// still reach the upstream.
			if jsonutil.StringAt(tool, "type") != "function" {
				continue
			}
			request.Tools = append(request.Tools, bridgeTool{
				Name:        jsonutil.StringAt(tool, "name"),
				Description: jsonutil.StringAt(tool, "description"),
				Schema:      tool["parameters"],
				Strict:      jsonutil.BoolAt(tool, "strict"),
			})
		}
		request.ToolChoice = decodeResponsesToolChoice(input["tool_choice"])
		request.ResponseFormat = jsonutil.MapAt(input, "text", "format")
		request.ParallelToolCalls = input["parallel_tool_calls"]
		request.PromptCacheKey = input["prompt_cache_key"]
		request.SafetyIdentifier = input["safety_identifier"]
		request.ServiceTier = input["service_tier"]
		request.Store = input["store"]

	case Anthropic:
		request.MaxTokens = input["max_tokens"]
		request.Stop = input["stop_sequences"]
		request.Reasoning = anthropicReasoning(input)
		blocks, err := decodeAnthropicBlocksChecked(input["system"])
		if err != nil {
			return request, fmt.Errorf("system: %w", err)
		}
		request.System = blocks
		for i, raw := range jsonutil.SliceAt(input, "messages") {
			message, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("messages[%d] must be an object", i)
			}
			role := jsonutil.StringAt(message, "role")
			// Some clients (e.g. Claude Code) inline system/developer messages
			// into the messages array instead of using the top-level system
			// field. Fold system messages into request.System and developer
			// messages into request.Developer rather than rejecting them.
			if role == "system" {
				blocks, err := decodeAnthropicBlocksChecked(message["content"])
				if err != nil {
					return request, fmt.Errorf("messages[%d]: %w", i, err)
				}
				request.System = append(request.System, blocks...)
				continue
			}
			if role == "developer" {
				blocks, err := decodeAnthropicBlocksChecked(message["content"])
				if err != nil {
					return request, fmt.Errorf("messages[%d]: %w", i, err)
				}
				request.Developer = append(request.Developer, blocks...)
				continue
			}
			if role != "user" && role != "assistant" {
				return request, fmt.Errorf("messages[%d] has unsupported role %q", i, role)
			}
			blocks, err := decodeAnthropicBlocksChecked(message["content"])
			if err != nil {
				return request, fmt.Errorf("messages[%d]: %w", i, err)
			}
			request.Messages = append(request.Messages, bridgeMessage{Role: role, Blocks: blocks})
		}
		for i, raw := range jsonutil.SliceAt(input, "tools") {
			tool, ok := raw.(map[string]any)
			if !ok {
				return request, fmt.Errorf("tools[%d] must be an object", i)
			}
			request.Tools = append(request.Tools, bridgeTool{
				Name:        jsonutil.StringAt(tool, "name"),
				Description: jsonutil.StringAt(tool, "description"),
				Schema:      tool["input_schema"],
			})
		}
		request.ToolChoice = decodeAnthropicToolChoice(input["tool_choice"])

	default:
		return request, fmt.Errorf("unsupported input protocol %q", protocol)
	}
	for i := range request.Messages {
		for j := range request.Messages[i].Blocks {
			block := &request.Messages[i].Blocks[j]
			if block.Kind != "tool_result" || protocol == Chat {
				continue
			}
			result, err := decodeToolImages(protocol, block.Result)
			if err != nil {
				return request, fmt.Errorf("messages[%d].tool_result[%d]: %w", i, j, err)
			}
			if _, images := result.([]bridgeBlock); images && block.CallID == "" {
				return request, fmt.Errorf("messages[%d]: tool image result requires a call ID", i)
			}
			block.Result = result
		}
	}
	return request, nil
}

// appendBridgeBlock adds a block decoded from a standalone Responses item. It
// joins the previous message when that has the same role and holds only tool
// calls or results, so consecutive function_call (or function_call_output)
// items form one turn; otherwise the block starts a new message.
func appendBridgeBlock(messages *[]bridgeMessage, role string, block bridgeBlock) {
	if len(*messages) > 0 {
		last := &(*messages)[len(*messages)-1]
		if last.Role == role && bridgeBlocksOnlyTools(last.Blocks) {
			last.Blocks = append(last.Blocks, block)
			return
		}
	}
	*messages = append(*messages, bridgeMessage{Role: role, Blocks: []bridgeBlock{block}})
}

// bridgeBlocksOnlyTools reports whether blocks is non-empty and holds only
// tool calls and tool results.
func bridgeBlocksOnlyTools(blocks []bridgeBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.Kind != "tool_call" && block.Kind != "tool_result" {
			return false
		}
	}
	return true
}

// encodeBridgeRequest validates a bridge request against the target protocol
// and encodes it as that protocol's request body. The target must be one of
// the three message protocols, and the Chat encoder can also fail on a history
// whose tool calls and results do not pair up.
func encodeBridgeRequest(protocol Protocol, request bridgeRequest) (map[string]any, error) {
	if err := validateBridgeRequest(protocol, request); err != nil {
		return nil, err
	}
	switch protocol {
	case Chat:
		return encodeChatRequest(request)
	case Responses:
		return encodeResponsesRequest(request), nil
	case Anthropic:
		return encodeAnthropicRequest(request), nil
	default:
		return nil, fmt.Errorf("unsupported output protocol %q", protocol)
	}
}

// validateBridgeRequest rejects content the target protocol cannot represent
// at all: a file in Responses instructions, and for Anthropic a file known
// only by its file_id.
func validateBridgeRequest(protocol Protocol, request bridgeRequest) error {
	if protocol == Responses {
		for _, block := range request.System {
			if block.Kind == "file" {
				return errors.New("file content is not valid inside Responses instructions; send it as a message input item")
			}
		}
	}
	blocks := make([]bridgeBlock, 0, len(request.System)+len(request.Developer))
	blocks = append(blocks, request.System...)
	blocks = append(blocks, request.Developer...)
	for _, message := range request.Messages {
		blocks = append(blocks, message.Blocks...)
	}
	for _, block := range blocks {
		if block.Kind == "file" && protocol == Anthropic && block.FileID != "" && block.Data == "" && block.URL == "" {
			return fmt.Errorf("file %q cannot be represented by Anthropic Messages without file data or a URL", block.FileID)
		}
	}
	return nil
}

// encodeChatRequest encodes a bridge request as a Chat Completions body.
//
// System and developer blocks become leading system and developer messages.
// Chat answers each tool call with a separate "tool" message that must follow
// the assistant message carrying the calls, so the encoder tracks pending
// calls: once every pending call has a result, the tool messages are emitted
// in call order, followed by user messages for any images the results carried
// (a tool message holds only text), and then by user content that arrived
// while the calls were pending. A history that breaks the call/result pairing
// is an error.
func encodeChatRequest(request bridgeRequest) (map[string]any, error) {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	jsonutil.Put(output, "temperature", request.Temperature)
	jsonutil.Put(output, "top_p", request.TopP)
	jsonutil.Put(output, "max_tokens", request.MaxTokens)
	jsonutil.Put(output, "stop", request.Stop)
	if effort := reasoningEffort(request.Reasoning); effort != nil {
		output["reasoning_effort"] = effort
	}
	jsonutil.Put(output, "response_format", chatResponseFormat(request.ResponseFormat))
	jsonutil.Put(output, "parallel_tool_calls", request.ParallelToolCalls)
	jsonutil.Put(output, "frequency_penalty", request.FrequencyPenalty)
	jsonutil.Put(output, "presence_penalty", request.PresencePenalty)
	jsonutil.Put(output, "seed", request.Seed)
	jsonutil.Put(output, "prompt_cache_key", request.PromptCacheKey)
	jsonutil.Put(output, "safety_identifier", request.SafetyIdentifier)
	jsonutil.Put(output, "service_tier", request.ServiceTier)
	jsonutil.Put(output, "store", request.Store)
	if request.Stream {
		output["stream_options"] = map[string]any{"include_usage": true}
	}

	messages := make([]any, 0, len(request.Messages)+1)
	if len(request.System) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": encodeChatBlocks(request.System)})
	}
	if len(request.Developer) > 0 {
		messages = append(messages, map[string]any{"role": "developer", "content": encodeChatBlocks(request.Developer)})
	}
	pending := make(map[string]bool)
	pendingResults := make(map[string]bridgeBlock)
	var pendingOrder []string
	var deferred [][]bridgeBlock
	// Chat has a reasoning_effort level but no field for an Anthropic thinking
	// budget. The budget the client named is emitted once, as
	// reasoning_budget_tokens on the first assistant message, so that decoding
	// this Chat body back into Anthropic can restore the budget exactly instead
	// of re-deriving it from the level (32000 used to come back as 8192).
	budgetCarried := false
	carryBudget := func(encoded map[string]any) {
		if budgetCarried {
			return
		}
		if budget := reasoningBudget(request.Reasoning); budget > 0 {
			encoded["reasoning_budget_tokens"] = budget
			budgetCarried = true
		}
	}
	flushDeferred := func() {
		for _, blocks := range deferred {
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": encodeChatBlocks(blocks)})
			}
		}
		deferred = nil
	}
	// A conversation can end before any assistant turn exists to carry the
	// budget. Attaching it to an empty assistant message keeps it on the wire
	// without inventing visible output; the upstream Chat endpoint treats the
	// empty turn as an assistant prefill.
	carryBudgetOnly := func() {
		if budgetCarried {
			return
		}
		if budget := reasoningBudget(request.Reasoning); budget <= 0 {
			return
		}
		encoded := map[string]any{"role": "assistant", "content": nil}
		carryBudget(encoded)
		if encoded["reasoning_budget_tokens"] == nil {
			return
		}
		messages = append(messages, encoded)
	}

	for i, message := range request.Messages {
		var content, reasoning, calls, results []bridgeBlock
		for _, block := range message.Blocks {
			switch block.Kind {
			case "reasoning":
				reasoning = append(reasoning, block)
			case "tool_call":
				calls = append(calls, block)
			case "tool_result":
				results = append(results, block)
			default:
				content = append(content, block)
			}
		}
		if message.Role == "assistant" {
			if len(pending) > 0 {
				return nil, fmt.Errorf("messages[%d]: assistant message appears before tool results for %s", i, strings.Join(missingToolIDs(pendingOrder, pending), ", "))
			}
			if len(results) > 0 {
				return nil, fmt.Errorf("messages[%d]: assistant message contains tool results", i)
			}
			encoded := map[string]any{"role": "assistant", "content": nil}
			carryBudget(encoded)
			if value, ok := bridgeReasoningText(reasoning); ok {
				encoded["reasoning_content"] = value
			}
			if len(content) > 0 {
				encoded["content"] = encodeChatBlocks(content)
			}
			if len(calls) > 0 {
				toolCalls := make([]any, 0, len(calls))
				for _, call := range calls {
					if call.ID == "" || call.Name == "" {
						return nil, fmt.Errorf("messages[%d]: tool call must contain id and name", i)
					}
					if pending[call.ID] {
						return nil, fmt.Errorf("messages[%d]: duplicate tool call id %q", i, call.ID)
					}
					pending[call.ID] = true
					pendingOrder = append(pendingOrder, call.ID)
					toolCalls = append(toolCalls, map[string]any{
						"id":   call.ID,
						"type": "function",
						"function": map[string]any{
							"name":      call.Name,
							"arguments": bridgeArgumentsJSON(call),
						},
					})
				}
				encoded["tool_calls"] = toolCalls
			}
			if len(content) > 0 || len(reasoning) > 0 || len(calls) > 0 {
				messages = append(messages, encoded)
				budgetCarried = true
			}
			continue
		}

		if len(calls) > 0 {
			return nil, fmt.Errorf("messages[%d]: user message contains tool calls", i)
		}
		for _, result := range results {
			if _, duplicate := pendingResults[result.CallID]; duplicate {
				return nil, fmt.Errorf("messages[%d]: duplicate tool result %q", i, result.CallID)
			}
			if !pending[result.CallID] {
				return nil, fmt.Errorf("messages[%d]: tool result %q has no pending tool call", i, result.CallID)
			}
			pendingResults[result.CallID] = result
			delete(pending, result.CallID)
		}
		if len(content) > 0 {
			if len(pendingOrder) > 0 || len(deferred) > 0 {
				deferred = append(deferred, content)
			} else {
				messages = append(messages, map[string]any{"role": "user", "content": encodeChatBlocks(content)})
			}
		}
		if len(pending) == 0 && len(pendingOrder) > 0 {
			var attachments []map[string]any
			for _, id := range pendingOrder {
				result := pendingResults[id]
				text, images := chatToolImages(result)
				if len(images) > 0 {
					attachments = append(attachments, map[string]any{"role": "user", "content": encodeChatBlocks(images)})
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": id,
					"content":      text,
				})
			}
			for _, attachment := range attachments {
				messages = append(messages, attachment)
			}
			flushDeferred()
			pendingOrder = nil
			clear(pendingResults)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("tool calls are missing results for %s", strings.Join(missingToolIDs(pendingOrder, pending), ", "))
	}
	flushDeferred()
	carryBudgetOnly()
	output["messages"] = messages

	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			function := map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  schemaOrDefault(tool.Schema),
			}
			if tool.Strict {
				function["strict"] = true
			}
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
		output["tools"] = tools
		if choice := encodeChatToolChoice(request.ToolChoice); choice != nil {
			output["tool_choice"] = choice
		}
	}
	return output, nil
}

// missingToolIDs lists the tool calls still waiting for a result, for an error
// message: in call order when that accounts for every pending call, and sorted
// otherwise.
func missingToolIDs(order []string, pending map[string]bool) []string {
	missing := make([]string, 0, len(pending))
	for _, id := range order {
		if pending[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == len(pending) {
		return missing
	}
	for id := range pending {
		found := false
		for _, current := range missing {
			if current == id {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

// responsesReasoning builds the Responses "reasoning" object. A nil result
// omits the field.
//
// A bare effort string, or a reasoning object from which reasoningEffort reads
// a level (a thinking block of type "enabled"/"adaptive" included), is
// projected onto "reasoning.effort". Copying such a block through verbatim
// (the old behavior) forwarded {"type":"adaptive"} and dropped the effort
// completely, so a client asking for "max" reached the upstream with no
// reasoning configuration at all.
//
// An object of type "disabled" yields nil and is omitted; one that yields no
// level is forwarded unchanged, and a bare "none"/"disabled" string yields nil.
func responsesReasoning(value any) any {
	if object, ok := value.(map[string]any); ok {
		if jsonutil.StringAt(object, "type") == "disabled" {
			return nil
		}
		// The object is reduced to the level reasoningEffort reads from it:
		// an "effort" or "reasoning_effort" the client stated, else the rung
		// of a thinking block's budget, else a level carried in "type".
		// Responses takes only the level here, so the object's other keys
		// (budget_tokens among them) are not forwarded.
		if effort := reasoningEffort(object); effort != nil {
			switch typed := effort.(type) {
			case string:
				return map[string]any{"effort": typed}
			case map[string]any:
				return typed
			default:
				return map[string]any{"effort": typed}
			}
		}
		return object
	}
	switch typed := value.(type) {
	case string:
		if effort, ok := normalizeEffortValue(typed); ok {
			if effort == nil {
				return nil
			}
			return map[string]any{"effort": effort}
		}
		return map[string]any{"effort": typed}
	case map[string]any:
		return typed
	default:
		return value
	}
}

// minResponsesMaxOutputTokens is the smallest max_output_tokens the Zen
// Responses endpoint accepts; anything lower is rejected with a 400.
const minResponsesMaxOutputTokens = 16

// floorResponsesMaxTokens keeps tiny downstream budgets (background titles,
// probes) from failing Zen responses validation. Larger budgets pass through
// untouched. Decoded JSON numbers are always float64, so no other numeric type
// can reach this point.
func floorResponsesMaxTokens(value any) any {
	if n, ok := value.(float64); ok && n > 0 && n < minResponsesMaxOutputTokens {
		return float64(minResponsesMaxOutputTokens)
	}
	return value
}

// encodeResponsesRequest encodes a bridge request as a Responses body.
// Instructions come from the system blocks and a leading developer message
// from the developer blocks. Each message becomes one or more input items:
// reasoning blocks become reasoning items (only when they carry a
// server-issued ID or encrypted content), text and images become content
// parts, files input_file parts, and tool calls and results standalone
// function_call and function_call_output items.
func encodeResponsesRequest(request bridgeRequest) map[string]any {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	jsonutil.Put(output, "temperature", request.Temperature)
	jsonutil.Put(output, "top_p", request.TopP)
	jsonutil.Put(output, "max_output_tokens", floorResponsesMaxTokens(request.MaxTokens))
	jsonutil.Put(output, "stop", request.Stop)
	jsonutil.Put(output, "metadata", request.Metadata)
	if len(request.System) > 0 {
		output["instructions"] = bridgeBlocksText(request.System)
	}
	if request.Reasoning != nil {
		if reasoning := responsesReasoning(request.Reasoning); reasoning != nil {
			output["reasoning"] = reasoning
		}
	}
	if format := responsesTextFormat(request.ResponseFormat); format != nil {
		output["text"] = map[string]any{"format": format}
	}
	jsonutil.Put(output, "parallel_tool_calls", request.ParallelToolCalls)
	// Native-fidelity knobs. Each is emitted only when the client sent it, so
	// a client that never sends them sees no behavior change, while OpenCode
	// turns ride prompt-cache affinity.
	jsonutil.Put(output, "prompt_cache_key", request.PromptCacheKey)
	jsonutil.Put(output, "safety_identifier", request.SafetyIdentifier)
	jsonutil.Put(output, "service_tier", request.ServiceTier)
	jsonutil.Put(output, "store", request.Store)

	items := make([]any, 0, len(request.Messages)+1)
	if len(request.Developer) > 0 {
		items = append(items, map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": bridgeBlocksText(request.Developer)}},
		})
	}
	for _, message := range request.Messages {
		var content []any
		flushContent := func() {
			if len(content) == 0 {
				return
			}
			items = append(items, map[string]any{
				"type":    "message",
				"role":    normalizeResponsesRole(message.Role),
				"content": content,
			})
			content = nil
		}
		for _, block := range message.Blocks {
			switch block.Kind {
			case "reasoning":
				flushContent()
				// Only replay reasoning the upstream actually issued. A block
				// that arrived as plain text (Chat/Anthropic history) has no
				// server-side id, and synthesizing one guarantees a stale
				// reasoning-reference rejection, which previously forced every
				// such turn through the strip-and-retry path.
				if item, ok := encodeResponsesReasoningInput(block); ok {
					items = append(items, item)
				}
			case "text":
				kind := "input_text"
				if message.Role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]any{"type": kind, "text": block.Text})
			case "image":
				url := block.URL
				if url == "" && block.Data != "" {
					url = "data:" + block.MediaType + ";base64," + block.Data
				}
				content = append(content, map[string]any{"type": "input_image", "image_url": url})
			case "file":
				item := map[string]any{"type": "input_file"}
				jsonutil.Put(item, "file_id", block.FileID)
				jsonutil.Put(item, "file_data", block.Data)
				jsonutil.Put(item, "filename", block.Filename)
				content = append(content, item)
			case "tool_call":
				flushContent()
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   block.ID,
					"name":      block.Name,
					"arguments": bridgeArgumentsJSON(block),
				})
			case "tool_result":
				flushContent()
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": block.CallID,
					"output":  responsesToolContent(block.Result),
				})
			}
		}
		flushContent()
	}
	output["input"] = items
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  schemaOrDefault(tool.Schema),
				"strict":      tool.Strict,
			})
		}
		output["tools"] = tools
		if choice := encodeResponsesToolChoice(request.ToolChoice); choice != nil {
			output["tool_choice"] = choice
		}
	}
	return output
}

// encodeAnthropicRequest encodes a bridge request as an Anthropic Messages
// body. System and developer blocks are merged into one system prompt, a
// missing max_tokens defaults to 4096, and adjacent same-role messages are
// joined into one, because Anthropic expects the turns to alternate. A "none"
// tool choice omits the tools entirely.
func encodeAnthropicRequest(request bridgeRequest) map[string]any {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	jsonutil.Put(output, "temperature", request.Temperature)
	jsonutil.Put(output, "top_p", request.TopP)
	jsonutil.Put(output, "stop_sequences", request.Stop)
	if request.MaxTokens == nil {
		output["max_tokens"] = 4096
	} else {
		output["max_tokens"] = request.MaxTokens
	}
	if thinking, outputConfig := anthropicThinking(request.Reasoning); thinking != nil {
		output["thinking"] = thinking
		if outputConfig != nil {
			output["output_config"] = outputConfig
		}
	}
	if len(request.System) > 0 || len(request.Developer) > 0 {
		instructions := append([]bridgeBlock(nil), request.System...)
		instructions = append(instructions, request.Developer...)
		output["system"] = encodeAnthropicBlocks(instructions)
	}

	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		content := encodeAnthropicBlocks(message.Blocks)
		if len(content) == 0 {
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "assistant"
		}
		if len(messages) > 0 {
			last, _ := messages[len(messages)-1].(map[string]any)
			if jsonutil.StringAt(last, "role") == role {
				last["content"] = append(jsonutil.AsSlice(last["content"]), content...)
				continue
			}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	output["messages"] = messages
	// Anthropic has no tool_choice "none" value. Omitting the tools is the
	// protocol-compatible representation of an explicitly disabled tool set.
	if len(request.Tools) > 0 && request.ToolChoice.Mode != "none" {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			tools = append(tools, map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": schemaOrDefault(tool.Schema),
			})
		}
		output["tools"] = tools
		if choice := encodeAnthropicToolChoice(request.ToolChoice); choice != nil {
			output["tool_choice"] = choice
		}
	}
	return output
}
