package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// writeJSON encodes v with the given status. Encoding errors after the header is
// written can only be logged by the caller's middleware; they are not recoverable.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the uniform error envelope: {"error": "..."}.
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// apiError422 is the uniform "unprocessable" envelope for state-dependent
// validation failures the client could not have caught from the request shape
// alone: {"error": msg, "code": code} so the frontend can branch on `code`
// without parsing prose (final-source guards A3/A4, audit 2026-07-16).
func apiError422(w http.ResponseWriter, code, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "code": code})
}

const maxJSONBody = 1 << 20 // 1 MiB

// decodeJSON strictly decodes the request body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	// Reject trailing garbage.
	if dec.More() {
		return errors.New("invalid JSON body: trailing data")
	}
	return nil
}
