package protocol

import (
	"fmt"
	"strings"

	"opencode2api/internal/jsonutil"
)

// Only image-bearing tool results enter this path. Keep all legacy text and
// opaque results unchanged, and reuse bridgeBlock rather than a second model.
func decodeToolImages(from Protocol, value any) (any, error) {
	parts, ok := value.([]any)
	if !ok {
		return value, nil
	}
	imageType, textType := "input_image", "input_text"
	if from == Anthropic {
		imageType, textType = "image", "text"
	}
	hasImage := false
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		hasImage = hasImage || jsonutil.StringAt(part, "type") == imageType
	}
	if !hasImage {
		return value, nil
	}
	for i, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content[%d] must be an object", i)
		}
		switch jsonutil.StringAt(part, "type") {
		case textType:
			if _, ok := part["text"].(string); !ok {
				return nil, fmt.Errorf("content[%d] requires text", i)
			}
		case imageType:
			if from == Anthropic {
				source := jsonutil.MapAt(part, "source")
				valid := false
				switch jsonutil.StringAt(source, "type") {
				case "url":
					valid = jsonutil.StringAt(source, "url") != "" && source["data"] == nil && source["file_id"] == nil
				case "base64":
					valid = jsonutil.StringAt(source, "data") != "" && strings.HasPrefix(jsonutil.StringAt(source, "media_type"), "image/") && source["url"] == nil && source["file_id"] == nil
				}
				if !valid {
					return nil, fmt.Errorf("content[%d] has an unsupported or conflicting image source", i)
				}
			} else if jsonutil.StringAt(part, "image_url") == "" || part["file_id"] != nil {
				return nil, fmt.Errorf("content[%d] requires an image URL without a file reference", i)
			}
		default:
			return nil, fmt.Errorf("content[%d] contains an unsupported block alongside tool images", i)
		}
	}
	if from == Anthropic {
		return decodeAnthropicBlocksChecked(value)
	}
	blocks, err := decodeOpenAIBlocksChecked(value)
	if err != nil {
		return nil, err
	}
	for i := range blocks {
		block := &blocks[i]
		if block.Kind != "image" || !strings.HasPrefix(block.URL, "data:") {
			continue
		}
		header, data, ok := strings.Cut(strings.TrimPrefix(block.URL, "data:"), ",")
		media, base64 := strings.CutSuffix(header, ";base64")
		if !ok || !base64 || !strings.HasPrefix(media, "image/") || data == "" {
			return nil, fmt.Errorf("content[%d] has an invalid image data URI", i)
		}
		block.URL, block.MediaType, block.Data = "", media, data
	}
	return blocks, nil
}

func chatToolImages(result bridgeBlock) (string, []bridgeBlock) {
	parts, ok := result.Result.([]bridgeBlock)
	if !ok {
		return bridgeToolResultContent(result.Result), nil
	}
	var text strings.Builder
	var images []bridgeBlock
	n := 0
	for _, part := range parts {
		if part.Kind == "text" {
			text.WriteString(part.Text)
			continue
		}
		n++
		marker := fmt.Sprintf("[tool_image call_id=%s index=%d]", bridgeArgumentsJSON(bridgeBlock{Arguments: result.CallID}), n)
		text.WriteString("\n" + marker + "\n")
		images = append(images, bridgeBlock{Kind: "text", Text: marker}, part)
	}
	return text.String(), images
}

func responsesToolContent(value any) any {
	parts, ok := value.([]bridgeBlock)
	if !ok {
		return bridgeToolResultContent(value)
	}
	content := make([]any, 0, len(parts))
	for _, part := range parts {
		if part.Kind == "text" {
			content = append(content, map[string]any{"type": "input_text", "text": part.Text})
		} else {
			url := part.URL
			if url == "" {
				url = "data:" + part.MediaType + ";base64," + part.Data
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": url})
		}
	}
	return content
}
