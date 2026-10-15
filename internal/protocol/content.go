package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
)

func decodeOpenAIBlocks(value any) []bridgeBlock {
	blocks, _ := decodeOpenAIBlocksChecked(value)
	return blocks
}

func decodeOpenAIBlocksChecked(value any) ([]bridgeBlock, error) {
	switch value := value.(type) {
	case string:
		if value == "" {
			return nil, nil
		}
		return []bridgeBlock{{Kind: "text", Text: value}}, nil
	case map[string]any:
		return decodeOpenAIBlocksChecked([]any{value})
	case []any:
		blocks := make([]bridgeBlock, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("OpenAI content block must be an object")
			}
			switch jsonutil.StringAt(part, "type") {
			case "text", "input_text", "output_text":
				blocks = append(blocks, bridgeBlock{Kind: "text", Text: jsonutil.StringAt(part, "text")})
			case "image_url":
				blocks = append(blocks, bridgeBlock{Kind: "image", URL: jsonutil.FirstString(jsonutil.StringAt(part, "image_url", "url"), jsonutil.StringAt(part, "image_url"))})
			case "input_image":
				blocks = append(blocks, bridgeBlock{Kind: "image", URL: jsonutil.StringAt(part, "image_url")})
			case "file", "input_file":
				file := jsonutil.MapAt(part, "file")
				blocks = append(blocks, bridgeBlock{
					Kind:      "file",
					FileID:    jsonutil.FirstString(jsonutil.StringAt(part, "file_id"), jsonutil.StringAt(file, "file_id")),
					Filename:  jsonutil.FirstString(jsonutil.StringAt(part, "filename"), jsonutil.StringAt(file, "filename")),
					MediaType: jsonutil.FirstString(jsonutil.StringAt(part, "media_type"), jsonutil.StringAt(file, "media_type")),
					Data:      jsonutil.FirstString(jsonutil.StringAt(part, "file_data"), jsonutil.StringAt(file, "file_data")),
				})
			default:
				return nil, fmt.Errorf("unsupported OpenAI content block type %q", jsonutil.StringAt(part, "type"))
			}
		}
		return blocks, nil
	default:
		if value == nil {
			return nil, nil
		}
		return nil, errors.New("unsupported OpenAI content value")
	}
}

func decodeAnthropicBlocks(value any) []bridgeBlock {
	blocks, _ := decodeAnthropicBlocksChecked(value)
	return blocks
}

func decodeAnthropicBlocksChecked(value any) ([]bridgeBlock, error) {
	if text, ok := value.(string); ok {
		if text == "" {
			return nil, nil
		}
		return []bridgeBlock{{Kind: "text", Text: text}}, nil
	}
	var blocks []bridgeBlock
	for _, raw := range jsonutil.AsSlice(value) {
		part, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("Anthropic content block must be an object")
		}
		switch jsonutil.StringAt(part, "type") {
		case "thinking":
			blocks = append(blocks, bridgeBlock{
				Kind:      "reasoning",
				Text:      jsonutil.StringAt(part, "thinking"),
				Signature: jsonutil.StringAt(part, "signature"),
			})
		case "redacted_thinking":
			blocks = append(blocks, bridgeBlock{Kind: "reasoning", Encrypted: jsonutil.StringAt(part, "data")})
		case "text":
			blocks = append(blocks, bridgeBlock{Kind: "text", Text: jsonutil.StringAt(part, "text")})
		case "image":
			source := jsonutil.MapAt(part, "source")
			if jsonutil.StringAt(source, "type") == "url" {
				blocks = append(blocks, bridgeBlock{Kind: "image", URL: jsonutil.StringAt(source, "url")})
			} else {
				blocks = append(blocks, bridgeBlock{Kind: "image", MediaType: jsonutil.StringAt(source, "media_type"), Data: jsonutil.StringAt(source, "data")})
			}
		case "document":
			source := jsonutil.MapAt(part, "source")
			if jsonutil.StringAt(source, "type") == "text" {
				blocks = append(blocks, bridgeBlock{Kind: "text", Text: jsonutil.FirstString(jsonutil.StringAt(source, "data"), jsonutil.StringAt(source, "text"))})
				continue
			}
			blocks = append(blocks, bridgeBlock{
				Kind:      "file",
				URL:       jsonutil.StringAt(source, "url"),
				MediaType: jsonutil.StringAt(source, "media_type"),
				Data:      jsonutil.FirstString(jsonutil.StringAt(source, "data"), jsonutil.StringAt(source, "text")),
				Filename:  jsonutil.StringAt(part, "title"),
			})
		case "tool_use":
			blocks = append(blocks, bridgeBlock{
				Kind:      "tool_call",
				ID:        jsonutil.StringAt(part, "id"),
				Name:      jsonutil.StringAt(part, "name"),
				Arguments: part["input"],
			})
		case "tool_result":
			blocks = append(blocks, bridgeBlock{
				Kind:    "tool_result",
				CallID:  jsonutil.StringAt(part, "tool_use_id"),
				Result:  part["content"],
				IsError: jsonutil.BoolAt(part, "is_error"),
			})
		default:
			return nil, fmt.Errorf("unsupported Anthropic content block type %q", jsonutil.StringAt(part, "type"))
		}
	}
	return blocks, nil
}

func decodeChatReasoning(message map[string]any) []bridgeBlock {
	value, exists := message["reasoning_content"]
	if !exists {
		value, exists = message["reasoning"]
	}
	text, ok := value.(string)
	if !exists || !ok {
		return nil
	}
	return []bridgeBlock{{
		Kind:      "reasoning",
		Text:      text,
		Signature: jsonutil.StringAt(message, "reasoning_signature"),
	}}
}

func decodeResponsesReasoning(item map[string]any) []bridgeBlock {
	var text strings.Builder
	for _, raw := range jsonutil.AsSlice(item["summary"]) {
		part, _ := raw.(map[string]any)
		text.WriteString(jsonutil.StringAt(part, "text"))
	}
	if content := item["content"]; content != nil {
		for _, raw := range jsonutil.AsSlice(content) {
			part, _ := raw.(map[string]any)
			text.WriteString(jsonutil.StringAt(part, "text"))
		}
	}
	encrypted := jsonutil.StringAt(item, "encrypted_content")
	if text.Len() == 0 && encrypted == "" {
		return nil
	}
	return []bridgeBlock{{
		Kind:      "reasoning",
		ID:        jsonutil.StringAt(item, "id"),
		Text:      text.String(),
		Encrypted: encrypted,
	}}
}

// encodeResponsesReasoningInput renders a reasoning block for a Responses
// request body. It reports false for blocks that carry neither a server-issued
// id nor encrypted content: those came from a protocol whose wire format has no
// reasoning reference (Chat text or Anthropic thinking), so replaying them
// under a synthesized id can only be rejected as a stale reference.
func encodeResponsesReasoningInput(block bridgeBlock) (map[string]any, bool) {
	if block.ID == "" && block.Encrypted == "" {
		return nil, false
	}
	return encodeResponsesReasoning(block, false), true
}

func encodeResponsesReasoning(block bridgeBlock, completed bool) map[string]any {
	id := block.ID
	if id == "" {
		id = identity.RandomID("rs", 12)
	}
	summary := []any{}
	if block.Text != "" {
		summary = append(summary, map[string]any{"type": "summary_text", "text": block.Text})
	}
	item := map[string]any{"id": id, "type": "reasoning", "summary": summary}
	if block.Encrypted != "" {
		item["encrypted_content"] = block.Encrypted
	}
	if completed {
		item["status"] = "completed"
	}
	return item
}

func bridgeReasoningText(blocks []bridgeBlock) (string, bool) {
	if len(blocks) == 0 {
		return "", false
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Text != "" {
			text.WriteString(block.Text)
		} else if block.Encrypted != "" {
			text.WriteString(anthropicRedactedThinkingPlaceholder)
		}
	}
	return text.String(), true
}

func bridgeReasoningSignature(blocks []bridgeBlock) string {
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Signature != "" {
			return blocks[i].Signature
		}
	}
	return ""
}

func encodeChatBlocks(blocks []bridgeBlock) any {
	if len(blocks) == 1 && blocks[0].Kind == "text" {
		return blocks[0].Text
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			url := block.URL
			if url == "" && block.Data != "" {
				url = "data:" + block.MediaType + ";base64," + block.Data
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case "file":
			file := map[string]any{}
			jsonutil.Put(file, "file_id", block.FileID)
			jsonutil.Put(file, "file_data", block.Data)
			jsonutil.Put(file, "filename", block.Filename)
			parts = append(parts, map[string]any{"type": "file", "file": file})
		}
	}
	return parts
}

func encodeAnthropicBlocks(blocks []bridgeBlock) []any {
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			if block.URL != "" {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": block.URL}})
			} else {
				parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": block.MediaType, "data": block.Data}})
			}
		case "file":
			var source map[string]any
			if block.URL != "" {
				source = map[string]any{"type": "url", "url": block.URL}
			} else if block.Data != "" {
				source = map[string]any{"type": "base64", "media_type": jsonutil.FirstString(block.MediaType, "application/octet-stream"), "data": block.Data}
			}
			if source != nil {
				part := map[string]any{"type": "document", "source": source}
				jsonutil.Put(part, "title", block.Filename)
				parts = append(parts, part)
			}
		case "tool_call":
			input := block.Arguments
			if input == nil {
				input = jsonutil.ParseJSONOrString(block.ArgumentsJSON)
			}
			parts = append(parts, map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": input})
		case "tool_result":
			content := block.Result
			if blocks, ok := content.([]bridgeBlock); ok {
				content = encodeAnthropicBlocks(blocks)
			}
			parts = append(parts, map[string]any{"type": "tool_result", "tool_use_id": block.CallID, "content": content, "is_error": block.IsError})
		case "reasoning":
			if block.Encrypted != "" {
				parts = append(parts, map[string]any{"type": "redacted_thinking", "data": block.Encrypted})
				continue
			}
			part := map[string]any{"type": "thinking", "thinking": block.Text, "signature": block.Signature}
			parts = append(parts, part)
		}
	}
	return parts
}

func bridgeArgumentsJSON(block bridgeBlock) string {
	if block.ArgumentsJSON != "" {
		return block.ArgumentsJSON
	}
	if block.Arguments == nil {
		return "{}"
	}
	data, err := json.Marshal(block.Arguments)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func bridgeToolResultContent(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if parts, ok := value.([]any); ok {
		var text strings.Builder
		allText := true
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok || jsonutil.StringAt(part, "type") != "text" {
				allText = false
				break
			}
			text.WriteString(jsonutil.StringAt(part, "text"))
		}
		if allText {
			return text.String()
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeChatToolChoice(value any) bridgeToolChoice {
	if mode, ok := value.(string); ok {
		return bridgeToolChoice{Mode: mode}
	}
	choice, _ := value.(map[string]any)
	if jsonutil.StringAt(choice, "type") == "function" {
		return bridgeToolChoice{Mode: "named", Name: jsonutil.StringAt(choice, "function", "name")}
	}
	return bridgeToolChoice{}
}

func decodeResponsesToolChoice(value any) bridgeToolChoice {
	if mode, ok := value.(string); ok {
		return bridgeToolChoice{Mode: mode}
	}
	choice, _ := value.(map[string]any)
	if jsonutil.StringAt(choice, "type") == "function" {
		return bridgeToolChoice{Mode: "named", Name: jsonutil.StringAt(choice, "name")}
	}
	return bridgeToolChoice{}
}

func decodeAnthropicToolChoice(value any) bridgeToolChoice {
	if mode, ok := value.(string); ok {
		if mode == "required" {
			mode = "any"
		}
		return decodeAnthropicToolChoice(map[string]any{"type": mode})
	}
	choice, _ := value.(map[string]any)
	switch jsonutil.StringAt(choice, "type") {
	case "auto":
		return bridgeToolChoice{Mode: "auto"}
	case "none":
		return bridgeToolChoice{Mode: "none"}
	case "any":
		return bridgeToolChoice{Mode: "required"}
	case "tool":
		return bridgeToolChoice{Mode: "named", Name: jsonutil.StringAt(choice, "name")}
	default:
		return bridgeToolChoice{}
	}
}

func encodeChatToolChoice(choice bridgeToolChoice) any {
	switch choice.Mode {
	case "named":
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
	case "auto", "none", "required":
		return choice.Mode
	default:
		return nil
	}
}

func encodeResponsesToolChoice(choice bridgeToolChoice) any {
	switch choice.Mode {
	case "named":
		return map[string]any{"type": "function", "name": choice.Name}
	case "auto", "none", "required":
		return choice.Mode
	default:
		return nil
	}
}

func encodeAnthropicToolChoice(choice bridgeToolChoice) any {
	switch choice.Mode {
	case "named":
		return map[string]any{"type": "tool", "name": choice.Name}
	case "required":
		return map[string]any{"type": "any"}
	case "auto":
		return map[string]any{"type": choice.Mode}
	default:
		return nil
	}
}
