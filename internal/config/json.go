package config

import (
	"bytes"
	"errors"
)

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
