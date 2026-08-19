package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Error is the API error envelope (components/schemas/Error).
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrUnavailable   = errors.New("unavailable")
	ErrUnprocessable = errors.New("unprocessable")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, Error{Code: code, Message: message})
}

// handleErr maps sentinel errors to the stable (status, code) pairs the
// OpenAPI contract declares.
func handleErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "resource.not_found", err.Error())
	case errors.Is(err, ErrConflict):
		writeErr(w, http.StatusConflict, "resource.conflict", err.Error())
	case errors.Is(err, ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "service.unavailable", err.Error())
	case errors.Is(err, ErrUnprocessable):
		writeErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// decodeBody reads a capped JSON request body into dst.
func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
