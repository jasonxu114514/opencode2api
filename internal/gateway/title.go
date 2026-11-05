package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"opencode2api/internal/httpx"
	"opencode2api/internal/jsonutil"
)

// titlePromptV1 opens the user message of a 1.x OpenCode title request.
const titlePromptV1 = "generate a title for this conversation"

// titlePromptsV2 are the system or developer prompt fragments that mark a 2.x
// OpenCode title request.
var titlePromptsV2 = []string{
	"you name conversations so the user can find them later in a list of past sessions",
	"you generate a descriptive title for this conversation",
	"you decide whether the session title still matches the conversation",
	"reply with a title and nothing else",
	"reply with a title for it and nothing else",
}

const (
	// defaultTitle is returned when no usable user text is found.
	defaultTitle = "New Session"
	// titleMaxWords and titleMaxRunes bound a synthesized title.
	titleMaxWords = 6
	titleMaxRunes = 40
	// titleMaxMessages is the largest message count a title request carries.
	titleMaxMessages = 3
)

// isTitleGenerationBody reports whether a raw JSON body is an OpenCode
// title-generation request.
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
// call rather than agent traffic.
//
// OpenCode sends title calls with no tools. The free tier accepts that form but
// answers 403 FreeTierError or 429 FreeUsageLimitError once an agent tool list
// is attached, so the gateway must not inject tools into it. To keep agent
// traffic from matching, a title request has no tools, at most
// titleMaxMessages messages, no assistant or tool turn, and a known title
// prompt in the role OpenCode uses for it.
func isTitleGenerationRequest(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		return false
	}
	if functions, ok := payload["functions"].([]any); ok && len(functions) > 0 {
		return false
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 || len(messages) > titleMaxMessages {
		return false
	}
	matched := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(jsonutil.StringAt(message, "role")))
		// A conversation turn means this is agent traffic, not a fresh title.
		if role == "assistant" || role == "tool" || role == "function" {
			return false
		}
		text := strings.ToLower(strings.TrimSpace(strings.Join(textFragments(message["content"]), " ")))
		if text == "" {
			continue
		}
		switch role {
		case "system", "developer":
			for _, prompt := range titlePromptsV2 {
				if strings.Contains(text, prompt) {
					matched = true
					break
				}
			}
		case "user":
			if strings.HasPrefix(text, titlePromptV1) {
				matched = true
			}
		}
	}
	return matched
}

// synthesizeTitle derives a short title from the first user message: its first
// line, cut to titleMaxWords words and titleMaxRunes runes.
func synthesizeTitle(payload map[string]any) string {
	userText := ""
	for _, raw := range jsonutil.SliceAt(payload, "messages") {
		message, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(jsonutil.StringAt(message, "role"))) != "user" {
			continue
		}
		if userText = strings.TrimSpace(strings.Join(textFragments(message["content"]), " ")); userText != "" {
			break
		}
	}
	// A 1.x request carries the conversation after the prompt line.
	if strings.HasPrefix(strings.ToLower(userText), titlePromptV1) {
		if index := strings.Index(userText, "\n"); index >= 0 {
			userText = strings.TrimSpace(userText[index+1:])
		}
	}
	firstLine, _, _ := strings.Cut(userText, "\n")
	words := strings.Fields(firstLine)
	if len(words) > titleMaxWords {
		words = words[:titleMaxWords]
	}
	title := strings.Join(words, " ")
	if utf8.RuneCountInString(title) > titleMaxRunes {
		title = string([]rune(title)[:titleMaxRunes])
	}
	title = strings.Trim(title, "\"' \t\r\n.")
	if title == "" {
		return defaultTitle
	}
	return title
}

// titleFallback answers a title request the upstream refused (429) or never
// reached with a locally synthesized title, so the client does not surface an
// error for a cosmetic call. It returns nil when the outcome needs no fallback;
// otherwise it closes lastResponse.
func titleFallback(logger *slog.Logger, model string, rawBody []byte, lastResponse *http.Response, lastErr error) *http.Response {
	if upstreamStatus(lastResponse) != http.StatusTooManyRequests && (lastResponse != nil || lastErr == nil) {
		return nil
	}
	var payload map[string]any
	_ = json.Unmarshal(rawBody, &payload)
	title := synthesizeTitle(payload)
	stream, _ := payload["stream"].(bool)
	if lastResponse != nil {
		httpx.DrainAndClose(lastResponse.Body)
	}
	logger.Info("title generation request rejected by upstream; returning a synthesized title", "component", "upstream", "event", "title_fallback_synthesized", "model", model, "title", title)
	return synthesizeTitleResponse(model, title, stream)
}

// synthesizeTitleResponse builds a 200 OpenAI Chat response carrying title,
// as one JSON completion or as an SSE stream ending in [DONE].
func synthesizeTitleResponse(model, title string, stream bool) *http.Response {
	var idBytes [8]byte
	_, _ = rand.Read(idBytes[:])
	id := "chatcmpl-title-" + hex.EncodeToString(idBytes[:])
	created := time.Now().Unix()
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}

	if !stream {
		completionTokens := utf8.RuneCountInString(title)
		data, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": title},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     0,
				"completion_tokens": completionTokens,
				"total_tokens":      completionTokens,
			},
		})
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp
	}

	chunk := func(delta map[string]any, finishReason any) []byte {
		data, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		})
		return data
	}
	var body bytes.Buffer
	for _, data := range [][]byte{
		chunk(map[string]any{"role": "assistant", "content": title}, nil),
		chunk(map[string]any{}, "stop"),
	} {
		body.WriteString("data: ")
		body.Write(data)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Body = io.NopCloser(&body)
	return resp
}
