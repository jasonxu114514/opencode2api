package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// migrateOAuthFields preserves legacy account settings while rejecting
// aliases that would otherwise overwrite each other during JSON decoding.
func migrateOAuthFields(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("configuration must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	seen := make(map[string]bool)
	changed := false
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid configuration JSON")
		}
		folded := strings.ToLower(name)
		if seen[folded] {
			return nil, errors.New("configuration fields must not be repeated")
		}
		seen[folded] = true
		canonical := name
		for _, names := range [][2]string{{"go_subscription", "go_oauth"}, {"zen_subscription", "zen_oauth"}} {
			if strings.EqualFold(name, names[0]) || strings.EqualFold(name, names[1]) {
				canonical = names[1]
				changed = changed || canonical != name
				break
			}
		}
		if _, exists := fields[canonical]; exists {
			return nil, errors.New("legacy and current OAuth configuration fields must not be combined")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid configuration JSON")
		}
		fields[canonical] = value
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errors.New("invalid configuration JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("configuration must contain one JSON object")
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(fields)
}

// stripJSONComments removes // and /* */ comments by overwriting each comment
// byte with a space but keeping newlines, so a position in the result is on
// the same line as in config.json. Comment markers inside JSON strings (for
// example, https:// URLs) are preserved. A leading UTF-8 BOM is stripped, and
// an unterminated /* comment is an error.
func stripJSONComments(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false
	lineComment := false
	blockComment := false

	for i := 0; i < len(data); i++ {
		current := data[i]
		if lineComment {
			if current == '\n' || current == '\r' {
				lineComment = false
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(data) && data[i+1] == '/' {
				out = append(out, ' ', ' ')
				i++
				blockComment = false
			} else if current == '\n' || current == '\r' {
				out = append(out, current)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		if inString {
			out = append(out, current)
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}

		switch {
		case current == '"':
			inString = true
			out = append(out, current)
		case current == '/' && i+1 < len(data) && data[i+1] == '/':
			lineComment = true
			out = append(out, ' ', ' ')
			i++
		case current == '/' && i+1 < len(data) && data[i+1] == '*':
			blockComment = true
			out = append(out, ' ', ' ')
			i++
		default:
			out = append(out, current)
		}
	}
	if blockComment {
		return nil, errors.New("unterminated block comment")
	}
	return out, nil
}
