// Package diag defines the structured diagnostic shared by core domain
// services and rendered by the API. Severity is info, warning, or error.
package diag

import (
	"encoding/json"
	"strings"
)

// Diagnostic is one machine-coded finding attached to a resource.
type Diagnostic struct {
	Code     string  `json:"code"`
	Severity string  `json:"severity"`
	Message  string  `json:"message"`
	Resource *string `json:"resource,omitempty"`
}

func newf(sev, code, msg string) Diagnostic {
	return Diagnostic{Code: code, Severity: sev, Message: msg}
}

func Info(code, msg string) Diagnostic    { return newf("info", code, msg) }
func Warning(code, msg string) Diagnostic { return newf("warning", code, msg) }
func Error(code, msg string) Diagnostic   { return newf("error", code, msg) }

// Res decorates a diagnostic with the resource it concerns.
func (d Diagnostic) Res(resource string) Diagnostic {
	d.Resource = &resource
	return d
}

func (d Diagnostic) IsError() bool { return d.Severity == "error" }

type diagnosticKey struct {
	code     string
	resource string
}

func keyOf(d Diagnostic) diagnosticKey {
	key := diagnosticKey{code: d.Code}
	if d.Resource != nil {
		key.resource = *d.Resource
	}
	return key
}

// Compact keeps the latest diagnostic for each code and resource while
// preserving the first occurrence's position.
func Compact(existing []Diagnostic) []Diagnostic {
	compacted := existing[:0]
	positions := make(map[diagnosticKey]int, len(existing))
	for _, diagnostic := range existing {
		key := keyOf(diagnostic)
		if position, ok := positions[key]; ok {
			compacted[position] = diagnostic
			continue
		}
		positions[key] = len(compacted)
		compacted = append(compacted, diagnostic)
	}
	return compacted
}

// Upsert replaces the diagnostic with the same code and resource in place,
// or appends it when no matching finding exists.
func Upsert(existing []Diagnostic, next Diagnostic) []Diagnostic {
	existing = Compact(existing)
	key := keyOf(next)
	for i := range existing {
		if keyOf(existing[i]) == key {
			existing[i] = next
			return existing
		}
	}
	return append(existing, next)
}

// Encode renders diagnostics for TEXT columns; nil becomes "[]" so readers
// can always unmarshal into a slice.
func Encode(ds []Diagnostic) string {
	if ds == nil {
		return "[]"
	}
	b, _ := json.Marshal(ds)
	return string(b)
}

// Decode parses a diagnostics column. Malformed input yields nil rather
// than an error: diagnostics are advisory, never load-bearing.
func Decode(s string) []Diagnostic {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var ds []Diagnostic
	if err := json.Unmarshal([]byte(s), &ds); err != nil {
		return nil
	}
	return ds
}

// HasError reports whether any diagnostic is error-severity.
func HasError(ds []Diagnostic) bool {
	for _, d := range ds {
		if d.IsError() {
			return true
		}
	}
	return false
}
