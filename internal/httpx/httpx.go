// Package httpx is the shared HTTP helper surface for the control plane and
// every first-party module handler: the structured error envelope, request
// body decoding, and the schema's canonical timestamp format.
package httpx

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
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

// WriteJSON writes v as the JSON response body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteErr writes a structured error response.
func WriteErr(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, Error{Code: code, Message: message})
}

// HandleErr maps sentinel errors to the stable (status, code) pairs the
// OpenAPI contract declares.
func HandleErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		WriteErr(w, http.StatusNotFound, "resource.not_found", err.Error())
	case errors.Is(err, ErrConflict):
		WriteErr(w, http.StatusConflict, "resource.conflict", err.Error())
	case errors.Is(err, ErrUnavailable):
		WriteErr(w, http.StatusServiceUnavailable, "service.unavailable", err.Error())
	case errors.Is(err, ErrUnprocessable):
		WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
	default:
		WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// DecodeBody reads a capped JSON request body into dst, rejecting unknown
// fields.
func DecodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// MustJSON marshals v, tolerating failure with an empty object.
func MustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// IsNoRows matches the sqlc-level no-row error.
func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// DBTime renders timestamps in the schema's canonical format.
func DBTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ParseDBTime parses the schema's canonical timestamp (RFC3339 fallback).
func ParseDBTime(v string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}
