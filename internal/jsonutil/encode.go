package jsonutil

import (
	"bytes"
	"encoding/json"
)

// Marshal encodes value the way the JavaScript clients this gateway imitates
// do. encoding/json escapes '<', '>' and '&' as \u003c, \u003e and \u0026 by
// default; JSON.stringify never does. A body carrying those escapes is
// byte-different from a real client's on every request whose text contains an
// angle bracket or ampersand, and tool descriptions and code snippets are full
// of both, so upstream request bodies go through here instead.
func Marshal(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
