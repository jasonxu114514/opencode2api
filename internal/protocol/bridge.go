package protocol

import (
	"strings"

	"opencode2api/internal/jsonutil"
)

// Placeholder reasoning texts. toolReasoningPlaceholder fills the reasoning a
// tool-call turn is missing, and anthropicRedactedThinkingPlaceholder stands
// in for encrypted reasoning wherever a readable text is required.
const (
	toolReasoningPlaceholder             = "tool call"
	anthropicRedactedThinkingPlaceholder = "[redacted thinking]"
)

// reasoningVendorHints are substrings of a model ID or upstream URL that
// identify the reasoning vendors whose endpoints reject tool-call turns
// without reasoning history (see normalizeToolReasoningHistory).
var reasoningVendorHints = [...]string{"moonshot", "kimi", "deepseek", "mimo", "xiaomimimo"}

// bridgeBlock is one content block of the protocol-neutral bridge model. Kind
// is "text", "image", "file", "reasoning", "tool_call" or "tool_result", and
// only the fields that kind uses are set. A tool call carries its input as
// decoded Arguments (from Anthropic) or as raw ArgumentsJSON (from Chat and
// Responses). A tool result's Result is the client's raw content, or a
// []bridgeBlock once decodeToolImages has parsed images out of it.
type bridgeBlock struct {
	Kind          string
	Text          string
	Signature     string
	Encrypted     string
	URL           string
	MediaType     string
	Data          string
	FileID        string
	Filename      string
	ID            string
	Name          string
	Arguments     any
	ArgumentsJSON string
	CallID        string
	Result        any
	IsError       bool
}

type bridgeMessage struct {
	Role   string
	Blocks []bridgeBlock
}

type bridgeTool struct {
	Name        string
	Description string
	Schema      any
	Strict      bool
}

// bridgeToolChoice is a decoded tool_choice. Mode is "auto", "none",
// "required" or "named" (with Name set), or empty when the client chose
// nothing; a mode the target protocol cannot express is not encoded.
type bridgeToolChoice struct {
	Mode string
	Name string
}

// bridgeRequest is a client request decoded into the protocol-neutral form
// every request encoder reads. System and Developer instructions stay apart so
// the Chat and Responses encoders can keep them distinct; the Anthropic
// encoder merges them. Reasoning holds the client's reasoning controls as an
// opaque value that reasoningEffort, responsesReasoning and anthropicThinking
// project onto each target.
type bridgeRequest struct {
	Model       string
	System      []bridgeBlock
	Developer   []bridgeBlock
	Messages    []bridgeMessage
	Tools       []bridgeTool
	ToolChoice  bridgeToolChoice
	Stream      bool
	Temperature any
	TopP        any
	MaxTokens   any
	Stop        any
	Reasoning   any
	Metadata    any
	// Sampling and response-shape knobs are carried across the bridge as
	// opaque values. The bridge used to drop them all, which made clients
	// believe (for example) that JSON mode had been honored. Each encoder now
	// re-emits the knobs its protocol can express: ResponseFormat and
	// ParallelToolCalls for Chat and Responses, the two penalties and Seed for
	// Chat only. A knob or format the target cannot express is omitted rather
	// than rejected, and the Anthropic encoder emits none of them.
	ResponseFormat    any
	ParallelToolCalls any
	FrequencyPenalty  any
	PresencePenalty   any
	Seed              any
	// Native-fidelity knobs, decoded from Chat and Responses bodies and
	// re-emitted only by the Chat and Responses encoders; the Anthropic
	// decoder and encoder ignore them. prompt_cache_key is the big one: native
	// OpenCode sends the session-sliced ID here and rides prompt-cache
	// affinity; without it every proxied turn pays full attention over the
	// whole history.
	PromptCacheKey   any
	SafetyIdentifier any
	ServiceTier      any
	Store            any
}

// Usage is the token accounting of one response in bridge form. Decoders
// normalize each protocol's usage object into it and encoders split it back
// out. Output is the completion output, Total the reported total (Input+Output
// when the upstream reports none), Cached and CacheCreation the cache-read and
// cache-write parts of Input, and Reasoning the reasoning tokens an
// OpenAI-style upstream reports as part of Output.
type Usage struct {
	// Input is the total prompt input, including cache reads and writes. This
	// matches OpenAI's prompt_tokens semantics and lets each output encoder
	// split the provider-specific cache fields exactly once.
	Input         int
	Output        int
	Total         int
	Cached        int
	CacheCreation int
	Reasoning     int
}

// bridgeResponse is a complete (non-streaming) response in protocol-neutral
// form, as decoded by decodeBridgeResponse or accumulated by CollapseStream.
type bridgeResponse struct {
	ID        string
	Model     string
	Text      string
	Reasoning []bridgeBlock
	Tools     []bridgeBlock
	Stop      string
	Error     string
	Usage     Usage
	Created   int64
}

// ConvertRequest converts a decoded client request body from one protocol to
// another. A same-protocol body is deep-cloned unchanged, so provider-specific
// fields survive; any other body is decoded into the bridge model and
// re-encoded for the target. The error reports content that is malformed or
// that the target protocol cannot represent.
func ConvertRequest(from, to Protocol, input map[string]any) (map[string]any, error) {
	if from == to {
		return jsonutil.CloneMap(input), nil
	}
	request, err := decodeBridgeRequest(from, input)
	if err != nil {
		return nil, err
	}
	return encodeBridgeRequest(to, request)
}

// normalizeResponsesRole maps a bridge message role onto a Responses message
// role: assistant stays, everything else becomes user. Bridge messages only
// ever carry user and assistant, so this is a guard rather than a conversion.
func normalizeResponsesRole(role string) string {
	if role == "assistant" {
		return "assistant"
	}
	return "user"
}

// schemaOrDefault substitutes an empty object schema for a tool that declared
// no parameters.
func schemaOrDefault(value any) any {
	if value == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return value
}

// chatResponseFormat converts a bridge-level response format into the OpenAI
// Chat Completions shape. Chat and Responses agree on the format type names but
// not on the json_schema nesting: Responses carries name/schema/strict inline,
// while Chat nests them under "json_schema". A value that has no Chat
// equivalent (the "text" type) is omitted instead of failing the request.
func chatResponseFormat(format any) any {
	object, ok := format.(map[string]any)
	if !ok {
		return nil
	}
	switch jsonutil.StringAt(object, "type") {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		if nested, ok := object["json_schema"].(map[string]any); ok {
			return map[string]any{"type": "json_schema", "json_schema": nested}
		}
		nested := make(map[string]any, 4)
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, exists := object[key]; exists {
				nested[key] = value
			}
		}
		if len(nested) == 0 {
			return nil
		}
		return map[string]any{"type": "json_schema", "json_schema": nested}
	default:
		return nil
	}
}

// responsesTextFormat converts a bridge-level response format into the object
// accepted by the Responses "text.format" field. A Chat-style nested
// json_schema is flattened into the inline shape Responses expects.
func responsesTextFormat(format any) any {
	object, ok := format.(map[string]any)
	if !ok {
		return nil
	}
	switch jsonutil.StringAt(object, "type") {
	case "text", "json_object":
		return object
	case "json_schema":
		if nested, ok := object["json_schema"].(map[string]any); ok {
			flattened := make(map[string]any, len(nested)+1)
			for key, value := range nested {
				flattened[key] = value
			}
			flattened["type"] = "json_schema"
			return flattened
		}
		return object
	default:
		return nil
	}
}

// reasoningEffort projects a bridge-level reasoning value onto the OpenAI Chat
// "reasoning_effort" field. A nil result omits the field.
//
// Precedence matters: a level the client stated explicitly (an "effort" or
// "reasoning_effort" key, whether or not it arrived next to a thinking block)
// wins over anything inferred from a thinking budget. Before this, an
// Anthropic request carrying both thinking.budget_tokens and
// output_config.effort threw the explicit effort away and reported the budget
// bucket instead, so a client asking for "max" was served as "high".
//
// Explicitly disabled reasoning remains distinct from an unset value so later
// defaults cannot turn it back on.
func reasoningEffort(value any) any {
	if object, ok := value.(map[string]any); ok {
		if jsonutil.StringAt(object, "type") == "disabled" {
			return "none"
		}
		if effort, ok := explicitEffort(object); ok {
			return effort
		}
		switch jsonutil.StringAt(object, "type") {
		case "enabled", "adaptive":
			return effortForThinkingBudget(jsonutil.IntAt(object, "budget_tokens"))
		default:
			if effort, ok := normalizeEffortValue(object["type"]); ok {
				return effort
			}
			return object["type"]
		}
	}
	if effort, ok := normalizeEffortValue(value); ok {
		return effort
	}
	return value
}

// reasoningDisabled reports whether a reasoning value explicitly disables it.
func reasoningDisabled(value any) bool {
	effort, ok := reasoningEffort(value).(string)
	return ok && effort == "none"
}

// explicitEffort reports the effort a client stated verbatim in a reasoning
// object and whether the object carried one at all. It looks at "effort" first
// and at "reasoning_effort" when "effort" states nothing; a key holding a blank
// or non-string value counts as stating nothing. A stated effort outranks a
// budget, including inside a thinking block that also has budget_tokens: budget
// 2048 plus effort "max" is a request for "max", not for "low".
//
// The second return is ok, not "effort != nil": the stated value nil means the
// client named "none"/"disabled" explicitly, which must clear an inferred level
// rather than fall through to it.
func explicitEffort(object map[string]any) (any, bool) {
	for _, key := range []string{"effort", "reasoning_effort"} {
		if value, exists := object[key]; exists {
			if effort, ok := normalizeEffortValue(value); ok {
				return effort, true
			}
		}
	}
	return nil, false
}

// normalizeEffortValue validates an effort a client stated explicitly. A
// non-blank string is passed through verbatim (case preserved) so unsupported
// levels are not silently rewritten or dropped; only "none"/"disabled" mean
// "no reasoning" and clear the value. Everything else, a blank string or any
// non-string value (nil included), reports ok=false so the caller can fall
// back to a budget-derived level.
func normalizeEffortValue(value any) (any, bool) {
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "none", "disabled":
		return "none", true
	case "":
		return nil, false
	}
	return value, true
}

// anthropicThinking maps a bridge-level reasoning value back onto the Anthropic
// "thinking" block plus the "output_config" that carries the effort natively.
// Both are nil when the value disables reasoning or says nothing about it.
//
// The budget reproduced here keeps the Anthropic payload self-consistent and
// mirrors what the inverse direction (effortForThinkingBudget) reads back, so a
// synthesized budget maps back to the same level, except that "minimal" comes
// back as "low". A budget the client originally named is restored verbatim
// rather than re-derived from the level, so an Anthropic -> Chat -> Anthropic
// round trip does not rewrite 32000 into 16384, the budget budgetForEffort
// synthesizes for the "xhigh" rung 32000 falls on.
//
// The emitted number is a float64 because that is the shape a decoded JSON
// number has, which keeps converted payloads indistinguishable from the ones
// that came in through a same-protocol passthrough.
func anthropicThinking(value any) (any, map[string]any) {
	if value == nil {
		return nil, nil
	}
	if reasoningDisabled(value) {
		return map[string]any{"type": "disabled"}, nil
	}
	budget := 0
	if object, ok := value.(map[string]any); ok {
		kind := jsonutil.StringAt(object, "type")
		// A budget is what makes the round trip lossless, so it is read before
		// the effort: a value that reached the bridge as a thinking block still
		// names the budget the client sent, and re-deriving one from the level
		// would fold 32000 back down to 16384, the budget of its "xhigh" rung.
		budget = jsonutil.IntAt(object, "budget_tokens")
		if effort, stated := explicitEffort(object); stated {
			// "none"/"disabled" clears the reasoning configuration entirely.
			if effort == nil {
				return nil, nil
			}
			return thinkingFor(effort, budget), map[string]any{"effort": effort}
		}
		if kind == "disabled" {
			return nil, nil
		}
		// "enabled"/"adaptive" are thinking modes, not levels: the level comes
		// from the budget, as in reasoningEffort, and a missing budget means
		// "high". Passing the mode through would send output_config.effort
		// "enabled", which no upstream accepts.
		if kind == "enabled" || kind == "adaptive" {
			effort := effortForThinkingBudget(budget)
			return thinkingFor(effort, budget), map[string]any{"effort": effort}
		}
		// A level carried in "type", as in Chat's {"reasoning":{"type":"high"}}.
		if effort, stated := normalizeEffortValue(object["type"]); stated {
			if effort == nil {
				return nil, nil
			}
			return thinkingFor(effort, budget), map[string]any{"effort": effort}
		}
		if budget > 0 {
			return thinkingFor(effortForThinkingBudget(budget), budget),
				map[string]any{"effort": effortForThinkingBudget(budget)}
		}
		value = object["type"]
	}
	effort, ok := normalizeEffortValue(value)
	if !ok {
		if number, isNumber := numericValue(value); isNumber {
			// A bare number is a budget: keep it and publish the level it maps
			// to, so no rung is invented.
			if number <= 0 {
				return nil, nil
			}
			return thinkingFor(effortForThinkingBudget(number), number),
				map[string]any{"effort": effortForThinkingBudget(number)}
		}
		// Not an effort string (for example a bool): nothing to say about
		// reasoning, so stay silent rather than inventing a level.
		return nil, nil
	}
	if effort == nil {
		// "none"/"disabled".
		return nil, nil
	}
	return thinkingFor(effort, budget), map[string]any{"effort": effort}
}

// thinkingFor builds the Anthropic "thinking" block for an effort, reusing the
// budget the client named when there is one and synthesizing the level's
// budgetForEffort budget otherwise.
func thinkingFor(effort any, budget int) map[string]any {
	if budget <= 0 {
		budget = budgetForEffort(effort)
	}
	return map[string]any{"type": "enabled", "budget_tokens": float64(budget)}
}

// reasoningBudget reports the thinking budget a bridge-level reasoning value
// names, or 0 when the value carries none. Only an object can name one: a bare
// effort string states a level, not a budget, and a bare number is not read as
// a budget here (only anthropicThinking treats it as one).
//
// The budget is normally the object's budget_tokens, which holds both an
// Anthropic thinking budget and one the Chat decoder read back from an
// assistant message's reasoning_budget_tokens. An object without it may still
// name a budget under its own "reasoning_budget_tokens" key, but that key is
// read only when the object also states an effort.
func reasoningBudget(value any) int {
	object, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	if budget := jsonutil.IntAt(object, "budget_tokens"); budget > 0 {
		return budget
	}
	if _, ok := explicitEffort(object); !ok {
		return 0
	}
	// A stated effort may carry its budget under the Chat field name.
	return numericValueOrZero(object["reasoning_budget_tokens"])
}

// withReasoningBudget attaches a budget to a bridge-level reasoning value,
// keeping any effort already present alongside it: an object is copied with
// budget_tokens set, and any other value becomes the "effort" of a new object.
// A non-positive budget leaves the value unchanged.
func withReasoningBudget(value any, budget int) any {
	if budget <= 0 {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		merged := make(map[string]any, len(typed)+1)
		for key, existing := range typed {
			merged[key] = existing
		}
		merged["budget_tokens"] = budget
		return merged
	default:
		return map[string]any{"budget_tokens": budget, "effort": value}
	}
}

// numericValue reads a number as an int, truncating any fraction. A decoded
// JSON body reports every number as float64; int and int64 are accepted too,
// so a value built in Go rather than decoded is read as well.
func numericValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case int64:
		return int(number), true
	default:
		return 0, false
	}
}

// numericValueOrZero is numericValue with a zero for anything that is not a
// number.
func numericValueOrZero(value any) int {
	number, _ := numericValue(value)
	return number
}

// budgetForEffort synthesizes a thinking budget for a level when an Anthropic
// body needs one the client never named: "minimal" and "low" get 1024 (the
// smallest budget_tokens Anthropic accepts), "medium" 4096, "high" 8192,
// "xhigh" 16384 and "max" 32768. It is the inverse of effortForThinkingBudget:
// each budget maps back to its own level, except that "minimal" comes back as
// "low". Any other value gets the "medium" budget.
func budgetForEffort(value any) int {
	text, _ := value.(string)
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 8192
	case "xhigh":
		return 16384
	case "max":
		return 32768
	default:
		return 4096
	}
}

// effortForThinkingBudget buckets an Anthropic thinking budget onto an OpenAI
// reasoning level: a positive budget up to 2048 is "low", 2049..8191
// "medium", 8192..16383 "high", 16384..32767 "xhigh" and 32768 or more
// "max". The rungs are ordered and pairwise distinct so that a budget a client
// chose deliberately stays distinguishable: before this, everything at or
// above 8192 collapsed to "high" and the ladder had no rung above it, so 32000
// and 8192 were indistinguishable and an Anthropic -> Chat -> Anthropic round
// trip rewrote 32000 into 8192.
//
// A missing budget keeps the historical "high": thinking:{type:"adaptive"}
// with no budget_tokens is the common shape, and it must not silently change
// level. A client that wants a higher rung states "xhigh" or "max" explicitly,
// and that level is taken as stated instead of being derived from a budget.
func effortForThinkingBudget(budget int) string {
	switch {
	case budget <= 0:
		return "high"
	case budget >= 32768:
		return "max"
	case budget >= 16384:
		return "xhigh"
	case budget > 8192:
		// 8193..16383 is still "high": the "xhigh" rung starts at 16384, the
		// budget budgetForEffort synthesizes for it.
		return "high"
	case budget >= 8192:
		// The threshold itself keeps its historical bucket.
		return "high"
	case budget > 2048:
		return "medium"
	default:
		return "low"
	}
}

// bridgeBlocksText concatenates the text of the "text" blocks and ignores
// every other kind.
func bridgeBlocksText(blocks []bridgeBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Kind == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

// asPrefix returns id in the target protocol's ID style, prefix plus "_". An
// id that already has that prefix is returned unchanged; otherwise any
// "chatcmpl_", "resp_" or "msg_" prefix is stripped first, so one upstream ID
// maps consistently onto every protocol.
func asPrefix(id, prefix string) string {
	if strings.HasPrefix(id, prefix+"_") {
		return id
	}
	for _, current := range []string{"chatcmpl_", "resp_", "msg_"} {
		id = strings.TrimPrefix(id, current)
	}
	return prefix + "_" + id
}
