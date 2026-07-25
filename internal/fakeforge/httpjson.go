package fakeforge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// marshalBody encodes v as a JSON request body, or returns http.NoBody for
// a nil v.
func marshalBody(v any) (io.Reader, error) {
	if v == nil {
		return http.NoBody, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// decodeBody decodes a successful response body into v.
func decodeBody(resp *http.Response, v any) error {
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decoding response body: %w", err)
	}
	return nil
}

// decodeError builds an error for a non-2xx response, reconstructing a
// known sentinel from the response's wire code when present.
func decodeError(resp *http.Response) error {
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("request failed with status %d", resp.StatusCode)
	}
	if sentinel := errorForCode(env.Code); sentinel != nil {
		// sentinel.Error() already carries env.Error's text (it is what the
		// server encoded it from), so returning it bare avoids doubling the
		// message; callers add their own request-specific context via %w.
		return sentinel
	}
	return fmt.Errorf("request failed with status %d: %s", resp.StatusCode, env.Error)
}
