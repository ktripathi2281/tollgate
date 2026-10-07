package api

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as a JSON response with the given status. It encodes
// the whole body before writing anything, so an encoding failure becomes a
// 500 instead of a truncated response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(Envelope{Error: EnvelopeError{
			Message: "The response could not be encoded.",
			Type:    TypeServer,
		}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failed write means the client has gone; there is nobody to tell.
	_, _ = w.Write(body)
}

// WriteError writes e as an OpenAI error response. Callers set any extra
// headers, such as Retry-After, before calling it.
func WriteError(w http.ResponseWriter, e *Error) {
	WriteJSON(w, e.Status, e.Envelope())
}
