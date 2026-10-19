package jsonutil

import (
	"encoding/json"
	"errors"
	"io"
)

// EnsureEOF reports an error when dec holds anything besides the value that
// was already decoded: a trailing JSON value is "multiple JSON values", and
// trailing garbage is returned as the decoder's own error. Anything up to EOF
// is fine, so whitespace after the value is accepted.
func EnsureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
