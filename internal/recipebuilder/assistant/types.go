package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jj-link/local-model-works/internal/recipe"
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
	ResolvedURL  string `json:"resolved_url,omitempty"`
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
	Mode               string                                             `json:"mode,omitempty"`
	Investigation      string                                             `json:"investigation,omitempty"`
	SourceInventory    []SourceFile                                       `json:"source_inventory,omitempty"`
	ReadSource         func(context.Context, string) (ContextFile, error) `json:"-"`
	RecordLinkedSource func(context.Context, ContextFile) error           `json:"-"`
	Context            []ContextFile                                      `json:"context"`
	Manifest           json.RawMessage                                    `json:"manifest"`
	Assets             []Asset                                            `json:"assets"`
	Questions          []Question                                         `json:"questions"`
	Instruction        string                                             `json:"instruction,omitempty"`
	Diagnostics        []Diagnostic                                       `json:"diagnostics,omitempty"`
	Model              string                                             `json:"model,omitempty"`
	RecipeSchema       json.RawMessage                                    `json:"recipe_schema"`
	ResultSchema       map[string]any                                     `json:"result_schema"`
	SourcePins         any                                                `json:"source_pins"`
	RunExcerpts        []RunExcerpt                                       `json:"run_excerpts,omitempty"`
	PendingProposal    json.RawMessage                                    `json:"pending_proposal,omitempty"`
	SourceStatus       string                                             `json:"source_status,omitempty"`
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

// SourceFile describes every pinned repository entry, including unreadable
// assets. ReadSource must return the complete file, never a truncated excerpt.
type SourceFile struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Readable bool   `json:"readable"`
	Reason   string `json:"reason,omitempty"`
}

type Adaptation struct {
	Path        string `json:"path"`
	Description string `json:"description"`
	Reason      string `json:"reason"`
}

type Procedure struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	Description          string           `json:"description"`
	Manifest             json.RawMessage  `json:"manifest"`
	Files                []File           `json:"files"`
	SelectedSourceAssets []AssetSelection `json:"selected_source_assets"`
	Questions            []Question       `json:"questions"`
	Evidence             []Evidence       `json:"evidence"`
	Summary              string           `json:"summary"`
	Adaptations          []Adaptation     `json:"adaptations"`
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
	Procedures           []Procedure      `json:"procedures,omitempty"`
	Adaptations          []Adaptation     `json:"adaptations,omitempty"`
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

var procedureIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateResult enforces transport-independent output bounds before provider
// data can be persisted as a proposal.
func ValidateResult(result Result) error {
	if len(result.Procedures) > 32 {
		return invalid("assistant.output_too_large", "proposal exceeds 32 documented procedures")
	}
	if len(result.Procedures) > 0 {
		if len(result.Files) != 0 || len(result.SelectedSourceAssets) != 0 || len(result.Questions) != 0 || len(result.Evidence) != 0 || len(result.Adaptations) != 0 {
			return invalid("assistant.output_invalid", "procedure proposals must not mix top-level recipe changes with procedure changes")
		}
		seen := make(map[string]bool, len(result.Procedures))
		total, files := 0, 0
		for _, procedure := range result.Procedures {
			if !procedureIDPattern.MatchString(procedure.ID) || strings.TrimSpace(procedure.Name) == "" || strings.TrimSpace(procedure.Description) == "" || seen[procedure.ID] {
				return invalid("assistant.output_invalid", "procedures require unique canonical IDs, names, and descriptions")
			}
			seen[procedure.ID] = true
			if err := ValidateResult(Result{Manifest: procedure.Manifest, Files: procedure.Files, SelectedSourceAssets: procedure.SelectedSourceAssets, Questions: procedure.Questions, Evidence: procedure.Evidence, Adaptations: procedure.Adaptations}); err != nil {
				return err
			}
			files += len(procedure.Files)
			for _, file := range procedure.Files {
				total += len(file.Content)
			}
		}
		if files > MaxGeneratedFiles || total > MaxGeneratedTotal {
			return invalid("assistant.output_too_large", "combined procedure files exceed proposal limits")
		}
		return nil
	}
	for _, adaptation := range result.Adaptations {
		if strings.TrimSpace(adaptation.Path) == "" || strings.TrimSpace(adaptation.Description) == "" || strings.TrimSpace(adaptation.Reason) == "" {
			return invalid("assistant.output_invalid", "adaptations require a path, description, and reason")
		}
	}
	for _, question := range result.Questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" {
			return invalid("assistant.output_invalid", "unresolved questions require an ID and a specific question")
		}
	}
	if len(result.Manifest) == 0 || !json.Valid(result.Manifest) || !strings.HasPrefix(strings.TrimSpace(string(result.Manifest)), "{") {
		return invalid("assistant.output_invalid", "proposal manifest must be valid JSON")
	}
	var manifest recipe.Manifest
	if err := json.Unmarshal(result.Manifest, &manifest); err != nil {
		return invalid("assistant.output_invalid", "proposal manifest fields have invalid types")
	}
	for _, workload := range manifest.Workloads {
		if workload.Upstream == nil {
			continue
		}
		if len(result.Files) != 0 || len(result.SelectedSourceAssets) != 0 || len(manifest.Assets) != 0 || len(manifest.Artifacts) != 0 || manifest.Prepare != nil || manifest.Verify != nil {
			return invalid("assistant.upstream_composition", "source-owned execution cannot include generated files, selected package assets, application-acquired artifacts, or extension scripts")
		}
		for _, other := range manifest.Workloads {
			if other.Upstream == nil || other.Image.Reference != "" || other.Image.Digest != "" || len(other.Command) != 0 || len(other.Args) != 0 || other.HostPreparation != nil {
				return invalid("assistant.upstream_composition", "source-owned execution cannot substitute managed images, launcher commands, or host preparation")
			}
		}
		break
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
	return value != "" && value == path.Clean(value) && value != "." && !strings.ContainsAny(value, "\\:\x00") && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "../")
}

func forbiddenPath(value string) bool {
	first := strings.ToLower(strings.Split(value, "/")[0])
	return first == ".git" || first == ".codex" || first == ".agent" || first == ".claude" || first == ".cursor"
}

func invalid(code, message string) error { return &Error{Code: code, Message: message} }
