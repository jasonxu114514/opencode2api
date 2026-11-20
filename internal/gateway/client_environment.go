package gateway

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

const maxClientEnvironmentValueLength = 4096

type clientEnvironmentContextKey struct{}

// withClientEnvironment consumes the gateway-only extension before routing or
// protocol conversion. A request owns one complete snapshot: partial client
// data must not inherit unrelated fields from the deployment configuration.
func (g *Gateway) withClientEnvironment(r *http.Request, payload map[string]any, protocol wire.Protocol) (*http.Request, bool, error) {
	var environment config.PromptEnvironmentConfig
	raw, explicit := payload["client_environment"]
	if explicit {
		var err error
		environment, err = decodeClientEnvironment(raw)
		if err != nil {
			return r, false, err
		}
		delete(payload, "client_environment")
	} else if native, found := nativeClientEnvironment(payload, protocol); found {
		environment = native
	} else {
		environment = copyPromptEnvironment(g.cfg.Upstream.PromptEnvironment)
	}
	ctx := context.WithValue(r.Context(), clientEnvironmentContextKey{}, environment)
	return r.WithContext(ctx), explicit, nil
}

// requestEnvironment returns the immutable snapshot shared by this request's
// retries and routes. Internal callers without an ingress snapshot use only
// the explicit deployment configuration, never the gateway host environment.
func (g *Gateway) requestEnvironment(ctx context.Context) *config.PromptEnvironmentConfig {
	if environment, ok := ctx.Value(clientEnvironmentContextKey{}).(config.PromptEnvironmentConfig); ok {
		return &environment
	}
	environment := copyPromptEnvironment(g.cfg.Upstream.PromptEnvironment)
	return &environment
}

func copyPromptEnvironment(environment *config.PromptEnvironmentConfig) config.PromptEnvironmentConfig {
	if environment == nil {
		return config.PromptEnvironmentConfig{}
	}
	copy := *environment
	if environment.IsGitRepo != nil {
		gitRepo := *environment.IsGitRepo
		copy.IsGitRepo = &gitRepo
	}
	return copy
}

func decodeClientEnvironment(raw any) (config.PromptEnvironmentConfig, error) {
	var environment config.PromptEnvironmentConfig
	fields, ok := raw.(map[string]any)
	if !ok {
		return environment, errors.New("client_environment must be an object")
	}
	for name, value := range fields {
		switch name {
		case "working_directory", "workspace_root", "platform":
			text, ok := value.(string)
			if !ok {
				return environment, fmt.Errorf("client_environment.%s must be a string", name)
			}
			switch name {
			case "working_directory":
				environment.WorkingDirectory = text
			case "workspace_root":
				environment.WorkspaceRoot = text
			case "platform":
				environment.Platform = text
			}
		case "is_git_repo":
			gitRepo, ok := value.(bool)
			if !ok {
				return environment, errors.New("client_environment.is_git_repo must be a boolean")
			}
			environment.IsGitRepo = &gitRepo
		default:
			return environment, errors.New("client_environment contains an unsupported field")
		}
	}
	err := normalizeClientEnvironment(&environment)
	return environment, err
}

func normalizeClientEnvironment(environment *config.PromptEnvironmentConfig) error {
	for name, value := range map[string]string{
		"working_directory": environment.WorkingDirectory,
		"workspace_root":    environment.WorkspaceRoot,
		"platform":          environment.Platform,
	} {
		if len(value) > maxClientEnvironmentValueLength {
			return fmt.Errorf("client_environment.%s must not exceed 4096 bytes", name)
		}
		for _, character := range value {
			if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' || character == '<' || character == '>' {
				return fmt.Errorf("client_environment.%s must not contain control characters, line separators, or angle brackets", name)
			}
		}
	}
	if err := environment.Normalize(); err != nil {
		return errors.New(strings.Replace(err.Error(), "upstream.prompt_environment.", "client_environment.", 1))
	}
	return nil
}

// nativeClientEnvironment reads declarations from instruction channels and
// standalone Codex environment updates. It never inspects assistant messages,
// tool results, or arbitrary prose for paths. Conflicting/malformed native
// declarations count as an unknown client snapshot, blocking config fallback.
func nativeClientEnvironment(payload map[string]any, protocol wire.Protocol) (config.PromptEnvironmentConfig, bool) {
	var environment config.PromptEnvironmentConfig
	found, valid := false, true
	addInstructions := func(content any) {
		for _, text := range clientEnvironmentTexts(content) {
			candidate, present, wellFormed := parseEnvironmentBlocks(text)
			if !present {
				continue
			}
			found = true
			if !wellFormed || !mergeClientEnvironment(&environment, candidate) {
				valid = false
			}
		}
	}
	var latest config.PromptEnvironmentConfig
	updated := false
	addUser := func(content any) {
		texts := clientEnvironmentTexts(content)
		if blocks, ok := content.([]any); ok && len(blocks) != len(texts) {
			return // Mixed images/tool results are not standalone declarations.
		}
		update, present, wellFormed := parseCodexEnvironment(strings.Join(texts, "\n"))
		if present {
			// A later explicit Codex update replaces the earlier environment,
			// including fields it does not supply; no session state is retained.
			latest, updated = config.PromptEnvironmentConfig{}, true
			if wellFormed {
				latest = update
			}
		}
	}
	field := "messages"
	switch protocol {
	case wire.Chat:
	case wire.Anthropic:
		addInstructions(payload["system"])
	case wire.Responses:
		addInstructions(payload["instructions"])
		field = "input"
		if input, ok := payload[field].(string); ok {
			addUser(input)
		}
	default:
		return environment, false
	}
	messages, _ := payload[field].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := message["type"].(string)
		if kind != "" && kind != "message" {
			continue
		}
		switch message["role"] {
		case "system", "developer":
			addInstructions(message["content"])
		case "user":
			addUser(message["content"])
		}
	}
	if updated {
		return latest, true
	}
	if !valid {
		return config.PromptEnvironmentConfig{}, true
	}
	return environment, found
}

// clientEnvironmentTexts deliberately excludes nested tool_result content in
// Anthropic user messages. Only direct text/input_text blocks are declarations.
func clientEnvironmentTexts(content any) []string {
	if text, ok := content.(string); ok {
		return []string{text}
	}
	blocks, _ := content.([]any)
	var texts []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" && block["type"] != "input_text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

func parseEnvironmentBlocks(text string) (config.PromptEnvironmentConfig, bool, bool) {
	var environment config.PromptEnvironmentConfig
	found, inside, valid := false, false, true
	var fence byte
	fenceLength := 0
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		// Literal examples in Markdown are not client declarations.
		if !inside && len(line) >= 3 && (line[0] == '`' || line[0] == '~') {
			length := 1
			for length < len(line) && line[length] == line[0] {
				length++
			}
			if fence == 0 && length >= 3 {
				fence, fenceLength = line[0], length
				continue
			}
			if line[0] == fence && length >= fenceLength && strings.TrimSpace(line[length:]) == "" {
				fence = 0
				continue
			}
		}
		if fence != 0 {
			continue
		}
		switch line {
		case "<env>":
			if inside {
				valid = false
			}
			found, inside = true, true
		case "</env>":
			if !inside {
				valid = false
			}
			found, inside = true, false
		default:
			if !inside {
				continue
			}
			label, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			var field string
			switch label {
			case "Working directory":
				field = "working_directory"
			case "Workspace root folder":
				field = "workspace_root"
			case "Is directory a git repo":
				field = "is_git_repo"
			case "Platform":
				field = "platform"
			default:
				continue
			}
			if !addNativeEnvironmentValue(&environment, field, strings.TrimSpace(value)) {
				valid = false
			}
		}
	}
	return environment, found, valid && !inside
}

func parseCodexEnvironment(text string) (config.PromptEnvironmentConfig, bool, bool) {
	var environment config.PromptEnvironmentConfig
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<environment_context>") {
		return environment, false, true
	}
	if !strings.HasSuffix(text, "</environment_context>") {
		return environment, true, false
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	if _, err := decoder.Token(); err != nil { // The exact opening tag was checked above.
		return environment, true, false
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return environment, true, false
		}
		switch item := token.(type) {
		case xml.StartElement:
			field := item.Name.Local
			if field == "cwd" {
				field = "working_directory"
			}
			if item.Name.Space != "" || field != "working_directory" && field != "workspace_root" && field != "platform" && field != "is_git_repo" {
				if err := decoder.Skip(); err != nil {
					return environment, true, false
				}
				continue
			}
			if len(item.Attr) != 0 {
				return environment, true, false
			}
			var value strings.Builder
			for {
				part, err := decoder.Token()
				if err != nil {
					return environment, true, false
				}
				if _, ended := part.(xml.EndElement); ended {
					break
				}
				characters, ok := part.(xml.CharData)
				if !ok || value.Len()+len(characters) > maxClientEnvironmentValueLength {
					return environment, true, false
				}
				value.Write(characters)
			}
			if !addNativeEnvironmentValue(&environment, field, value.String()) {
				return environment, true, false
			}
		case xml.EndElement:
			_, err := decoder.Token()
			return environment, true, err == io.EOF
		case xml.CharData:
			if strings.TrimSpace(string(item)) != "" {
				return environment, true, false
			}
		default:
			return environment, true, false
		}
	}
}

func addNativeEnvironmentValue(environment *config.PromptEnvironmentConfig, field, value string) bool {
	if len(value) > maxClientEnvironmentValueLength {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' || character == '<' || character == '>' {
			return false
		}
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "unknown") {
		return true
	}
	var candidate config.PromptEnvironmentConfig
	switch field {
	case "working_directory":
		candidate.WorkingDirectory = value
	case "workspace_root":
		candidate.WorkspaceRoot = value
	case "platform":
		candidate.Platform = value
	case "is_git_repo":
		var gitRepo bool
		switch strings.ToLower(value) {
		case "yes", "true":
			gitRepo = true
		case "no", "false":
		default:
			return false
		}
		candidate.IsGitRepo = &gitRepo
	}
	return normalizeClientEnvironment(&candidate) == nil && mergeClientEnvironment(environment, candidate)
}

// mergeClientEnvironment combines consistent native declarations, not config
// defaults. Different known values make the complete snapshot ambiguous.
func mergeClientEnvironment(environment *config.PromptEnvironmentConfig, candidate config.PromptEnvironmentConfig) bool {
	for _, pair := range [][2]*string{
		{&environment.WorkingDirectory, &candidate.WorkingDirectory},
		{&environment.WorkspaceRoot, &candidate.WorkspaceRoot},
		{&environment.Platform, &candidate.Platform},
	} {
		if *pair[0] != "" && *pair[1] != "" && *pair[0] != *pair[1] {
			return false
		}
		if *pair[1] != "" {
			*pair[0] = *pair[1]
		}
	}
	if candidate.IsGitRepo != nil {
		if environment.IsGitRepo != nil && *environment.IsGitRepo != *candidate.IsGitRepo {
			return false
		}
		gitRepo := *candidate.IsGitRepo
		environment.IsGitRepo = &gitRepo
	}
	return true
}
