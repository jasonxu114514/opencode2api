// Package httpx provides shared HTTP response and transport helpers.
package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteJSON writes value as a JSON response with the given status, setting the
// Content-Type header first.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	WriteJSONStatus(w, status, value)
}

// WriteJSONStatus writes value as a JSON body with the given status without
// touching the headers, so a caller can set a Content-Type or Cache-Control of
// its own first. An encoding error cannot be reported: the status is already
// committed, and the body is simply cut short.
func WriteJSONStatus(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteSSE writes one server-sent event: an optional "id:" line when id is
// positive, an optional "event:" line when event is not empty, and the JSON
// encoding of value as the data. It returns an error only for the data: a
// value that cannot be encoded, or a data line that cannot be written.
func WriteSSE(w http.ResponseWriter, event string, id uint64, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if id > 0 {
		_, _ = fmt.Fprintf(w, "id: %d\n", id)
	}
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
