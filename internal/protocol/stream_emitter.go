package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

// bridgeStreamTool accumulates one tool call while a stream is encoded. Key is
// the parser's stream key, ID and ItemID the call and output-item identifiers,
// Arguments the full argument text so far and EmittedArguments how much of it
// has reached the client. Index is the Chat index or Responses output index,
// and AnthropicIndex the content block index; Started records whether the tool
// was opened downstream.
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

// bridgeStreamEncoder turns bridge stream events into one downstream protocol's
// SSE stream. It opens the response on the first start or content event (or on
// Finish when the stream carried none), tracks which text, reasoning and tool
// blocks are open so each protocol's block ordering is respected, and records
// usage as it goes.
type bridgeStreamEncoder struct {
	w                  io.Writer
	flush              http.Flusher
	target             Protocol
	model              string
	id                 string
	created            int64
	started            bool
	done               bool
	stop               string
	usage              Usage
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
}

// bridgeStreamEmitter builds an encoder for the target protocol. The response
// ID is taken from the first event that carries one and the model from the
// last event that carries one, so the constructor's model is only a fallback.
func bridgeStreamEmitter(writer io.Writer, flusher http.Flusher, target Protocol, model string) *bridgeStreamEncoder {
	return &bridgeStreamEncoder{
		w:       writer,
		flush:   flusher,
		target:  target,
		model:   model,
		created: time.Now().Unix(),
		tools:   map[string]*bridgeStreamTool{},
	}
}

// Emit encodes one bridge stream event for the target protocol. The response
// is opened before the first start or content event. "error" writes an in-band
// error event and then returns errStreamUpstreamFailure; "done" finishes the
// stream through Finish.
func (emitter *bridgeStreamEncoder) Emit(event bridgeStreamEvent) error {
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
			if emitter.target != Responses && event.Text == "" {
				event.Text = anthropicRedactedThinkingPlaceholder
			}
			if emitter.target == Responses && event.Text == "" {
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
		if err := emitter.emitError(jsonutil.FirstString(event.Error, "upstream stream failed"), jsonutil.FirstString(event.ErrorType, "upstream_error")); err != nil {
			return err
		}
		return errStreamUpstreamFailure
	case "done":
		return emitter.Finish()
	}
	return nil
}

// emitError writes an error in the target protocol's stream form: an "error"
// event for Anthropic, a response.failed event for Responses, and an error
// object followed by [DONE] for Chat.
func (emitter *bridgeStreamEncoder) emitError(message, errorType string) error {
	switch emitter.target {
	case Anthropic:
		return emitter.sse("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": errorType, "message": message},
		})
	case Responses:
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

// tool returns the state for a stream tool key, creating it with fresh IDs and
// the next index on first sight. An empty key gets a position-based one, so
// each unnamed event starts its own tool.
func (emitter *bridgeStreamEncoder) tool(key string) *bridgeStreamTool {
	if key == "" {
		key = fmt.Sprintf("tool-%d", len(emitter.order))
	}
	if current := emitter.tools[key]; current != nil {
		return current
	}
	tool := &bridgeStreamTool{
		Key:    key,
		ID:     identity.RandomID("call", 12),
		ItemID: identity.RandomID("fc", 12),
		Index:  len(emitter.order),
	}
	emitter.tools[key] = tool
	emitter.order = append(emitter.order, key)
	return tool
}

// anyToolStarted reports whether at least one tool block was actually opened
// downstream. Tools stuck without a name never start; their stop promise must
// not be advertised as a tool stop.
func (emitter *bridgeStreamEncoder) anyToolStarted() bool {
	for _, key := range emitter.order {
		if tool := emitter.tools[key]; tool != nil && tool.Started {
			return true
		}
	}
	return false
}

// start opens the response in the target protocol's shape: a role chunk for
// Chat, message_start for Anthropic, and response.created plus
// response.in_progress for Responses. It is a no-op once started.
func (emitter *bridgeStreamEncoder) start() error {
	if emitter.started {
		return nil
	}
	emitter.started = true
	if emitter.id == "" {
		emitter.id = identity.RandomID("resp", 12)
	}
	switch emitter.target {
	case Chat:
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
	case Anthropic:
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
				"usage":         anthropicUsage(Usage{}),
			},
		})
	case Responses:
		response := emitter.responsesBase("in_progress", []any{})
		if err := emitter.sse("response.created", map[string]any{"type": "response.created", "response": response, "sequence_number": emitter.nextSequence()}); err != nil {
			return err
		}
		return emitter.sse("response.in_progress", map[string]any{"type": "response.in_progress", "response": response, "sequence_number": emitter.nextSequence()})
	default:
		return fmt.Errorf("unsupported stream target %q", emitter.target)
	}
}

// emitText writes a text delta, closing the reasoning block first for
// Anthropic and Responses, whose content blocks are sequential. Anthropic
// opens a text content block and Responses a message output item with a text
// part, each on the first delta; Chat just takes the delta.
func (emitter *bridgeStreamEncoder) emitText(delta string) error {
	if emitter.target == Anthropic || emitter.target == Responses {
		if err := emitter.finishReasoning(); err != nil {
			return err
		}
	}
	switch emitter.target {
	case Chat:
		return emitter.chatChunk(map[string]any{"content": delta}, nil)
	case Anthropic:
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
	case Responses:
		if !emitter.textOpen {
			emitter.textOpen = true
			emitter.textItemID = identity.RandomID("msg", 12)
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

// emitReasoning writes a reasoning delta into the open reasoning block,
// opening it first if needed. Responses reports the text as a summary delta.
func (emitter *bridgeStreamEncoder) emitReasoning(delta string) error {
	if err := emitter.startReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case Chat:
		return emitter.chatChunk(map[string]any{"reasoning_content": delta}, nil)
	case Anthropic:
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": emitter.reasoningIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})
	case Responses:
		return emitter.sse("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "delta": delta, "item_id": emitter.reasoningItemID, "output_index": emitter.reasoningOutput, "summary_index": 0, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

// emitReasoningSignature writes a reasoning signature delta. Chat and Anthropic
// have a field for it; Responses has none, so the signature is dropped there.
func (emitter *bridgeStreamEncoder) emitReasoningSignature(signature string) error {
	if err := emitter.startReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case Chat:
		return emitter.chatChunk(map[string]any{"reasoning_signature": signature}, nil)
	case Anthropic:
		return emitter.sse("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": emitter.reasoningIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": signature},
		})
	}
	return nil
}

// startReasoning opens the reasoning block, once: a thinking content block for
// Anthropic, a reasoning output item with a summary part for Responses, and
// nothing at all for Chat, which has no block to open. It also does nothing
// after the block has been closed.
func (emitter *bridgeStreamEncoder) startReasoning() error {
	if emitter.reasoningOpen || emitter.reasoningClosed {
		return nil
	}
	emitter.reasoningOpen = true
	switch emitter.target {
	case Anthropic:
		emitter.reasoningIndex = emitter.nextAnthropic
		emitter.nextAnthropic++
		return emitter.sse("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         emitter.reasoningIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
	case Responses:
		emitter.reasoningItemID = identity.RandomID("rs", 12)
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

// finishReasoning closes the open reasoning block. Anthropic gets a
// content_block_stop, and Responses the summary done events plus the completed
// output item, which carries the encrypted content when the upstream sent any.
func (emitter *bridgeStreamEncoder) finishReasoning() error {
	if !emitter.reasoningOpen || emitter.reasoningClosed {
		return nil
	}
	emitter.reasoningClosed = true
	text := emitter.reasoning.String()
	switch emitter.target {
	case Anthropic:
		return emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": emitter.reasoningIndex})
	case Responses:
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

// startTool opens a tool call downstream once its name is non-empty, closing
// the reasoning block and, for Anthropic, the open text block first. A tool
// with no name stays unstarted, so a phantom call is never announced. Unlike
// usableToolBlocks, this check does not trim the name.
func (emitter *bridgeStreamEncoder) startTool(tool *bridgeStreamTool) error {
	if tool.Started || tool.Name == "" {
		return nil
	}
	if emitter.target == Responses || emitter.target == Anthropic {
		if err := emitter.finishReasoning(); err != nil {
			return err
		}
	}
	if emitter.target == Anthropic && emitter.textOpen {
		if err := emitter.sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": emitter.textIndex}); err != nil {
			return err
		}
		emitter.textOpen = false
	}
	tool.Started = true
	switch emitter.target {
	case Chat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": tool.Index,
			"id":    tool.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      tool.Name,
				"arguments": "",
			},
		}}}, nil)
	case Responses:
		tool.Index = emitter.nextOutput
		emitter.nextOutput++
		item := map[string]any{"id": tool.ItemID, "type": "function_call", "status": "in_progress", "arguments": "", "call_id": tool.ID, "name": tool.Name}
		return emitter.sse("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.Index, "item": item, "sequence_number": emitter.nextSequence()})
	case Anthropic:
		tool.AnthropicIndex = emitter.nextAnthropic
		emitter.nextAnthropic++
		return emitter.sse("content_block_start", map[string]any{
			"type": "content_block_start", "index": tool.AnthropicIndex,
			"content_block": map[string]any{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]any{}},
		})
	}
	return nil
}

// emitToolDelta writes a tool argument delta in the target protocol's form.
// The caller is responsible for having started the tool first.
func (emitter *bridgeStreamEncoder) emitToolDelta(tool *bridgeStreamTool, delta string) error {
	if delta == "" {
		return nil
	}
	switch emitter.target {
	case Chat:
		return emitter.chatChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    tool.Index,
			"function": map[string]any{"arguments": delta},
		}}}, nil)
	case Responses:
		return emitter.sse("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "delta": delta, "item_id": tool.ItemID, "output_index": tool.Index, "sequence_number": emitter.nextSequence()})
	case Anthropic:
		return emitter.sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": tool.AnthropicIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
	}
	return nil
}

// emitPendingToolArguments writes the argument text a tool has accumulated but
// not yet sent. A tool that has not started holds its arguments until it does,
// so nothing is lost when the name arrives late.
func (emitter *bridgeStreamEncoder) emitPendingToolArguments(tool *bridgeStreamTool) error {
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

// Finish closes the response in the target protocol's shape: the remaining
// text, reasoning and tool blocks, the stop reason, and the usage report. The
// stop reason defaults to "tool_calls" when any tool call was seen and "stop"
// otherwise, and a tool stop with no started tool is demoted to "stop" so the
// client does not wait for a call it never received. Finish is idempotent.
func (emitter *bridgeStreamEncoder) Finish() error {
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
	if isToolStop(emitter.stop) && !emitter.anyToolStarted() {
		// Same phantom-tool-call guard as encodeBridgeResponse, for the
		// streaming path: a tool_use stop reason with zero tool_use
		// content blocks ends the client turn silently.
		emitter.stop = "stop"
	}
	if err := emitter.finishReasoning(); err != nil {
		return err
	}
	switch emitter.target {
	case Chat:
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

	case Anthropic:
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

	case Responses:
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
		completed := encodeBridgeResponse(Responses, response)
		return emitter.sse("response.completed", map[string]any{"type": "response.completed", "response": completed, "sequence_number": emitter.nextSequence()})
	}
	return nil
}

// finishResponsesItems closes the open text item and every tool item with the
// done events a Responses client expects. A tool without arguments is closed
// with "{}", so the item is still executable.
func (emitter *bridgeStreamEncoder) finishResponsesItems() error {
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

// chatChunk writes one Chat chunk with the given delta and finish_reason.
func (emitter *bridgeStreamEncoder) chatChunk(delta map[string]any, finish any) error {
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

// responsesBase builds the response object that Responses lifecycle events
// carry.
func (emitter *bridgeStreamEncoder) responsesBase(status string, output []any) map[string]any {
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

// nextSequence returns the next Responses sequence_number.
func (emitter *bridgeStreamEncoder) nextSequence() int {
	sequence := emitter.sequence
	emitter.sequence++
	return sequence
}

// sse writes value as the data of one SSE event.
func (emitter *bridgeStreamEncoder) sse(eventName string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return emitter.rawSSE(eventName, string(data))
}

// rawSSE writes one SSE event and flushes it, so the client sees each event as
// it is produced. An empty eventName omits the event line, as Chat streams do.
func (emitter *bridgeStreamEncoder) rawSSE(eventName, data string) error {
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

// mergeBridgeUsage folds a usage report into the accumulated usage. A non-zero
// field replaces the stored one, Total keeps the larger of the two, and Total
// is finally raised to at least Input+Output so a report that leaves it out
// still adds up.
func mergeBridgeUsage(destination *Usage, source Usage) {
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
