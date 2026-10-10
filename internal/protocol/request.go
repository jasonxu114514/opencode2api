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
// either case.
func PrepareRequest(from, to Protocol, input map[string]any, upstreamURL string) (map[string]any, error) {
	output, err := ConvertRequest(from, to, input)
	if err != nil {
		return nil, err
	}
	normalizeToolReasoningHistory(to, jsonutil.StringAt(output, "model"), upstreamURL, output)
	return output, nil
}

// ForcedEffort applies an operator-configured thinking level to a prepared
// upstream body, so a client can be served at a fixed level without the client
// cooperating.
//
// A level the client stated explicitly always wins. That is what the "forced"
// level is for: replacing a level nobody actually asked for, either because the
// request said nothing about reasoning or because the conversion had to derive
// one from thinking.budget_tokens. It is applied to the finished upstream body
// rather than to the bridge value because the same-protocol path never goes
// through the bridge, and only there can the client's own body be seen.
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

// clientEffortExplicit reports whether an upstream body already carries a level
// the client stated, as opposed to one the gateway derived from a budget.
//
// The shapes differ per protocol, and each one mirrors how that protocol
// expressed the level natively: Anthropic states it in output_config.effort (or
// a top-level effort), Chat in a bare reasoning_effort string, and Responses in
// reasoning.effort. Anything else in those slots is a structural value the
// conversion produced, which a forced level is allowed to replace.
func clientEffortExplicit(protocol Protocol, body map[string]any) bool {
	switch protocol {
	case Chat:
		_, ok := body["reasoning_effort"].(string)
		return ok
	case Anthropic:
		if effort, ok := jsonutil.AnyAt(body, "output_config", "effort").(string); ok && strings.TrimSpace(effort) != "" {
			return true
		}
		effort, ok := body["effort"].(string)
		return ok && strings.TrimSpace(effort) != ""
	case Responses:
		_, ok := jsonutil.AnyAt(body, "reasoning", "effort").(string)
		return ok
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
// upstream rejects the request it was just told to run at a higher level.
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

// normalizeToolReasoningHistory applies only to endpoints that are known to
// require reasoning replay, or to requests that explicitly enable reasoning.
// Normalizing the target shape makes the behavior independent of the client
// protocol used to reach the gateway.
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

func isReasoningVendorIdentifier(value string) bool {
	value = strings.ToLower(value)
	for _, hint := range reasoningVendorHints {
		if strings.Contains(value, hint) {
			return true
		}
	}
	return false
}

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
// request invalid. A legacy reasoning string is promoted when available.
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
// clients can legitimately send each of those shapes.
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
// block and was reported downstream as the budget bucket "high".
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
				// level the client originally named.
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
			if !ok || jsonutil.StringAt(tool, "type") != "function" {
				return request, fmt.Errorf("tools[%d] must be a function tool", i)
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
			// field. Fold them into the system prompt rather than rejecting.
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
	// Anthropic's reasoning controls belong to a whole conversation, but Chat
	// carries a single reasoning_effort for the request. The budget the client
	// named is emitted once, on the first assistant message, so that decoding
	// this Chat body back into Anthropic can restore the level exactly instead
	// of re-deriving it from a bucket (32000 used to come back as 8192).
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
	// without inventing visible output; Chat clients treat the empty turn as an
	// assistant prefill.
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

// responsesReasoning builds the Responses "reasoning" object.
//
// A bare effort string, or a thinking-style block that carries budget_tokens
// but no effort, is projected onto "reasoning.effort". Copying such a block
// through verbatim (the old behavior) forwarded {"type":"adaptive"} and dropped
// the effort completely, so a client asking for "max" reached the upstream with
// no reasoning configuration at all.
func responsesReasoning(value any) any {
	if object, ok := value.(map[string]any); ok {
		if jsonutil.StringAt(object, "type") == "disabled" {
			return object
		}
		// "effort" is authoritative here for the same reason as in
		// encodeChatRequest: it either came from the client verbatim or was
		// derived from the budget by reasoningEffort.
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

func encodeResponsesRequest(request bridgeRequest) map[string]any {
	output := map[string]any{"model": request.Model, "stream": request.Stream}
	jsonutil.Put(output, "temperature", request.Temperature)
	jsonutil.Put(output, "top_p", request.TopP)
	jsonutil.Put(output, "max_output_tokens", request.MaxTokens)
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
	// ponytail: native-fidelity knobs. Omit-when-absent: clients that never
	// send them see zero behavior change; opencode turns ride cache affinity.
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
