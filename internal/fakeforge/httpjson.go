package fakeforge

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// errorEnvelope is the JSON body returned for non-2xx responses on the
// provider REST and control API surfaces.
type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// decodeJSON decodes the request body into v, writing a 400 response and
// returning false on failure so callers can return immediately.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("empty request body"))
		return false
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("decoding request body: %w", err))
		return false
	}
	return true
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes err as a JSON errorEnvelope with the given status
// code, attaching a wire code when err matches a known sentinel.
func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorEnvelope{Error: err.Error(), Code: codeForError(err)})
}
