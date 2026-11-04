package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"opencode2api/internal/jsonutil"
)

// Legacy OpenCode v1 title prompt prefix in a user message.
const titlePromptLegacy = "generate a title for this conversation"

// OpenCode v2 title generation signatures in system / developer messages.
const (
	titlePromptV2Name    = "you name conversations so the user can find them later in a list of past sessions"
	titlePromptV2Desc    = "you generate a descriptive title for this conversation"
	titlePromptV2Auto    = "you decide whether the session title still matches the conversation"
	titlePromptV2Rule    = "reply with a title and nothing else"
	titlePromptV2RuleAlt = "reply with a title for it and nothing else"
)

// isTitleGenerationBody checks if raw JSON body is an OpenCode title-generation request.
func isTitleGenerationBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return isTitleGenerationRequest(payload)
}

// isTitleGenerationRequest reports whether payload is OpenCode's session-title
// generation call rather than agent traffic (PR #102, Issue #164).
//
// OpenCode issues title calls with an EMPTY tool list. The upstream free-tier gate
// accepts that tool-free form but rejects the same call with 403 FreeTierError or
// 429 FreeUsageLimitError once an agent tool list is attached. Therefore, the gateway
// must not inject tools into title requests.
//
// To prevent false positives on user agent requests:
// 1. Tool lists must be absent or empty.
// 2. Message count must be at most 3 without assistant or tool turns.
// 3. Known OpenCode title prompt signatures must match in their expected roles.
func isTitleGenerationRequest(payload map[string]any) bool {
	if payload == nil {
		return false
	}

	// 1. Tool list must be absent or empty
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		return false
	}
	if functions, ok := payload["functions"].([]any); ok && len(functions) > 0 {
		return false
	}

	// 2. Must have 1 to 3 messages
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 || len(messages) > 3 {
		return false
	}

	hasSystemSignature := false
	hasLegacyUserSignature := false

	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(jsonutil.StringAt(msg, "role")))
		// Any in-progress conversation turn means this is NOT a fresh title request
		if role == "assistant" || role == "tool" || role == "function" {
			return false
		}

		text := strings.ToLower(strings.TrimSpace(strings.Join(textFragments(msg["content"]), " ")))
		if text == "" {
			continue
		}

		if role == "system" || role == "developer" {
			if strings.Contains(text, titlePromptV2Name) ||
				strings.Contains(text, titlePromptV2Desc) ||
				strings.Contains(text, titlePromptV2Auto) ||
				(strings.Contains(text, titlePromptV2Rule) || strings.Contains(text, titlePromptV2RuleAlt)) {
				hasSystemSignature = true
			}
		} else if role == "user" {
			// Legacy v1 title generation: user message starts with the command prompt
			if strings.HasPrefix(text, titlePromptLegacy) {
				hasLegacyUserSignature = true
			}
		}
	}

	return hasSystemSignature || hasLegacyUserSignature
}

// synthesizeTitle extracts the conversation topic from the payload's user message
// and formats a clean, compact title suitable for OpenCode sessions.
func synthesizeTitle(payload map[string]any) string {
	if payload == nil {
		return "New Session"
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return "New Session"
	}

	userText := ""
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(jsonutil.StringAt(msg, "role"))) == "user" {
			userText = strings.TrimSpace(strings.Join(textFragments(msg["content"]), " "))
			if userText != "" {
				break
			}
		}
	}

	if userText == "" {
		return "New Session"
	}

	// Strip legacy prompt wrapper if present
	lower := strings.ToLower(userText)
	if strings.HasPrefix(lower, titlePromptLegacy) {
		idx := strings.Index(userText, "\n")
		if idx >= 0 && idx < len(userText)-1 {
			userText = strings.TrimSpace(userText[idx+1:])
		}
	}

	// Clean whitespace and linebreaks
	lines := strings.Split(userText, "\n")
	firstLine := strings.TrimSpace(lines[0])
	words := strings.Fields(firstLine)
	if len(words) == 0 {
		return "New Session"
	}

	// Take up to 6 words
	if len(words) > 6 {
		words = words[:6]
	}
	title := strings.Join(words, " ")

	// Unicode rune-safe truncation up to 40 runes
	if utf8.RuneCountInString(title) > 40 {
		runes := []rune(title)
		title = string(runes[:40])
	}

	// Strip surrounding quotes or trailing periods
	title = strings.Trim(title, `"'`+" \t\r\n.")
	if title == "" {
		return "New Session"
	}
	return title
}

// synthesizeTitleResponse builds an OpenAI-compliant 200 OK HTTP response
// carrying the synthesized title, supporting both non-streaming and SSE streaming.
func synthesizeTitleResponse(model, title string, stream bool) *http.Response {
	var idBytes [8]byte
	_, _ = rand.Read(idBytes[:])
	reqID := "chatcmpl-title-" + hex.EncodeToString(idBytes[:])
	now := time.Now().Unix()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
	}

	if !stream {
		respBody := map[string]any{
			"id":      reqID,
			"object":  "chat.completion",
			"created": now,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": title,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": utf8.RuneCountInString(title),
				"total_tokens":      10 + utf8.RuneCountInString(title),
			},
		}
		data, _ := json.Marshal(respBody)
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp
	}

	// SSE streaming format
	var buf bytes.Buffer
	chunk1 := map[string]any{
		"id":      reqID,
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"delta": map[string]any{
					"role":    "assistant",
					"content": title,
				},
				"finish_reason": nil,
			},
		},
	}
	chunk2 := map[string]any{
		"id":      reqID,
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	d1, _ := json.Marshal(chunk1)
	d2, _ := json.Marshal(chunk2)
	buf.WriteString("data: ")
	buf.Write(d1)
	buf.WriteString("\n\ndata: ")
	buf.Write(d2)
	buf.WriteString("\n\ndata: [DONE]\n\n")

	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Body = io.NopCloser(&buf)
	return resp
}
