package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	MaxResponseBytes      = 4 << 20
	MaxGeneratedFiles     = 256
	MaxGeneratedFileBytes = 256 << 10
	MaxGeneratedTotal     = 2 << 20
)

// Provider produces inert proposal data. It has no installation, deployment,
// filesystem, or process-execution capability.
type Provider interface {
	Generate(context.Context, Request, func(string)) (Result, error)
}

type ContextFile struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	Origin       string `json:"origin,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	Content      string `json:"content"`
}

type Asset struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Content    string `json:"content,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	Origin     string `json:"origin,omitempty"`
}

type AssetSelection struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Question struct {
	ID       string `json:"id"`
	Path     string `json:"path,omitempty"`
	Question string `json:"question"`
	Answer   string `json:"answer,omitempty"`
}

type Diagnostic struct {
	ID          string `json:"id"`
	Code        string `json:"code"`
	Path        string `json:"path,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type Request struct {
	Context      []ContextFile   `json:"context"`
	Manifest     json.RawMessage `json:"manifest"`
	Assets       []Asset         `json:"assets"`
	Questions    []Question      `json:"questions"`
	Instruction  string          `json:"instruction,omitempty"`
	Diagnostics  []Diagnostic    `json:"diagnostics,omitempty"`
	Model        string          `json:"model,omitempty"`
	RecipeSchema json.RawMessage `json:"recipe_schema"`
	ResultSchema map[string]any  `json:"result_schema"`
	SourcePins   any             `json:"source_pins"`
	RunExcerpts  []RunExcerpt    `json:"run_excerpts,omitempty"`
}

type RunExcerpt struct {
	RunID        string `json:"run_id"`
	DeploymentID string `json:"deployment_id"`
	Rank         int32  `json:"rank"`
	Stream       string `json:"stream"`
	Offset       uint64 `json:"offset"`
	ByteCount    int    `json:"byte_count"`
	Content      string `json:"content"`
	SourceSHA256 string `json:"source_sha256"`
}

type File struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	SourcePath string `json:"source_path,omitempty"`
}

type Evidence struct {
	Path         string `json:"path"`
	SourcePath   string `json:"source_path,omitempty"`
	SHA256       string `json:"sha256"`
	SourceCommit string `json:"source_commit,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
}

type Result struct {
	Manifest             json.RawMessage  `json:"manifest"`
	Files                []File           `json:"files"`
	SelectedSourceAssets []AssetSelection `json:"selected_source_assets"`
	Questions            []Question       `json:"questions"`
	Evidence             []Evidence       `json:"evidence"`
	Summary              string           `json:"summary,omitempty"`
}

type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ValidateResult enforces transport-independent output bounds before provider
// data can be persisted as a proposal.
func ValidateResult(result Result) error {
	if len(result.Manifest) == 0 || !json.Valid(result.Manifest) {
		return invalid("assistant.output_invalid", "proposal manifest must be valid JSON")
	}
	if len(result.Files) > MaxGeneratedFiles {
		return invalid("assistant.output_too_large", fmt.Sprintf("proposal has %d files; maximum is %d", len(result.Files), MaxGeneratedFiles))
	}
	seen := make(map[string]struct{}, len(result.Files))
	total := 0
	for _, file := range result.Files {
		if !canonicalRelative(file.Path) {
			return invalid("assistant.output_path_invalid", "generated file path is not canonical and relative: "+file.Path)
		}
		if forbiddenPath(file.Path) {
			return invalid("assistant.output_path_forbidden", "generated file path is reserved: "+file.Path)
		}
		if _, exists := seen[file.Path]; exists {
			return invalid("assistant.output_path_duplicate", "generated file path is duplicated: "+file.Path)
		}
		seen[file.Path] = struct{}{}
		if !utf8.ValidString(file.Content) {
			return invalid("assistant.output_invalid", "generated files must contain valid UTF-8")
		}
		size := len(file.Content)
		if size > MaxGeneratedFileBytes {
			return invalid("assistant.output_too_large", "generated file exceeds the 256 KiB limit: "+file.Path)
		}
		total += size
		if total > MaxGeneratedTotal {
			return invalid("assistant.output_too_large", "generated files exceed the 2 MiB combined limit")
		}
	}
	return nil
}

func canonicalRelative(value string) bool {
	return value != "" && value == path.Clean(value) && value != "." && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../")
}

func forbiddenPath(value string) bool {
	first := strings.ToLower(strings.Split(value, "/")[0])
	return first == ".git" || first == ".codex" || first == ".agent" || first == ".claude" || first == ".cursor"
}

func invalid(code, message string) error { return &Error{Code: code, Message: message} }
