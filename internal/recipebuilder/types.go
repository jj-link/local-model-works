package recipebuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// AssetSelection pairs a retained inventory path with its exact content hash.
// A hash alone cannot distinguish identical bytes at different filenames.
type AssetSelection struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Origin string `json:"origin,omitempty"`
}

// Candidate is one inventoried, content-addressed source file.
type Candidate struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Binary     bool   `json:"binary,omitempty"`
	Origin     string `json:"origin"`
	SourcePath string `json:"source_path,omitempty"`
}

const (
	OriginSource    = "source"
	OriginGenerated = "generated"
)

const (
	PhaseInspect  = "inspect"
	PhaseGenerate = "generate"
	PhaseResolve  = "resolve"
	PhaseValidate = "validate"
	PhasePackage  = "package"
	PhaseInstall  = "install"
	PhaseLaunch   = "launch"
)

// Diagnostic is one structured finding owned by the server.
type Diagnostic struct {
	ID              string `json:"id"`
	Code            string `json:"code"`
	Severity        string `json:"severity"`
	Path            string `json:"path,omitempty"`
	Message         string `json:"message"`
	Phase           string `json:"phase"`
	Blocking        bool   `json:"blocking"`
	Remediation     string `json:"remediation"`
	SourcePath      string `json:"source_path,omitempty"`
	SourceLine      int    `json:"source_line,omitempty"`
	Resource        string `json:"resource,omitempty"`
	Retryable       bool   `json:"retryable"`
	Dismissible     bool   `json:"dismissible"`
	Acknowledgement string `json:"acknowledgement,omitempty"`
}

// DiagnosticID derives a stable identity from the canonical finding tuple.
// Repeated schema codes at different locations stay distinct.
func DiagnosticID(diag Diagnostic, operationID string) string {
	identity := []byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%s|%s",
		diag.Code, diag.Phase, diag.Path, diag.Message,
		diag.SourcePath, diag.Resource, diag.SourceLine, diag.Remediation, operationID))
	sum := sha256.Sum256(identity)
	return hex.EncodeToString(sum[:])[:24]
}

// Operation is a reserved mutation owning the draft while active.
type Operation struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Phase         string `json:"phase,omitempty"`
	Message       string `json:"message,omitempty"`
	StartedAt     string `json:"started_at"`
	UpdatedAt     string `json:"updated_at"`
	PreviousState string `json:"previous_state"`
}

// Question is a server-owned proposal question plus its accepted answer.
type Question struct {
	ID       string `json:"id"`
	Path     string `json:"path,omitempty"`
	Question string `json:"question"`
	Answer   string `json:"answer,omitempty"`
}

// ProposalFile is one generated UTF-8 package asset.
type ProposalFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	SourcePath string `json:"source_path,omitempty"`
}

// Evidence points at exact source lines supporting a generated decision.
type Evidence struct {
	Path         string `json:"path"`
	SourcePath   string `json:"source_path,omitempty"`
	SHA256       string `json:"sha256"`
	SourceCommit string `json:"source_commit,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
}

// Proposal is model- or operator-suggested content, inert until accepted.
type Proposal struct {
	ID                   string           `json:"id"`
	BaseVersion          int64            `json:"base_version"`
	ProviderID           string           `json:"provider_id,omitempty"`
	ProviderVersion      string           `json:"provider_version,omitempty"`
	Model                string           `json:"model,omitempty"`
	RunID                string           `json:"run_id,omitempty"`
	Manifest             json.RawMessage  `json:"manifest"`
	Files                []ProposalFile   `json:"files"`
	SelectedSourceAssets []AssetSelection `json:"selected_source_assets"`
	Questions            []Question       `json:"questions"`
	Evidence             []Evidence       `json:"evidence"`
	Summary              string           `json:"summary,omitempty"`
	Diagnostics          []Diagnostic     `json:"diagnostics"`
	PreviewSHA256        string           `json:"preview_sha256,omitempty"`
}

// ResolvedReference is server-owned evidence for one reference input.
type ResolvedReference struct {
	Path          string `json:"path"`
	InputIdentity string `json:"input_identity"`
	ResolvedValue string `json:"resolved_value"`
	Origin        string `json:"origin"`
	VerifiedAt    string `json:"verified_at"`
	EvidenceNote  string `json:"evidence_note,omitempty"`
}

// ContextFile is one reviewed source excerpt selected for assistance.
type ContextFile struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	Origin       string `json:"origin,omitempty"`
	SourceCommit string `json:"source_commit,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
}

// ContextExclusion records one path intentionally left out of the candidate
// store so the UI can list included/excluded paths and bytes.
type ContextExclusion struct {
	Path      string `json:"path"`
	Reason    string `json:"reason"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ChangeContext freezes the saved baseline; the draft source remains the target pin.
type ChangeContext struct {
	Kind                  string           `json:"kind"`
	RepositoryID          string           `json:"repository_id,omitempty"`
	BaseRecipeDigest      string           `json:"base_recipe_digest,omitempty"`
	ExpectedCurrentDigest string           `json:"expected_current_digest,omitempty"`
	BaseSource            *GitSource       `json:"base_source,omitempty"`
	BaseCommit            string           `json:"base_commit,omitempty"`
	BaseTree              string           `json:"base_tree,omitempty"`
	BaseManifest          json.RawMessage  `json:"base_manifest,omitempty"`
	BaseAssets            []AssetSelection `json:"base_assets,omitempty"`
	BaseCandidates        []Candidate      `json:"base_candidates,omitempty"`
	BaseSourceStatus      string           `json:"base_source_status"`
	BaseSourceError       string           `json:"base_source_error,omitempty"`
	DeploymentID          string           `json:"deployment_id,omitempty"`
}

// Draft is the persisted correction workspace document.
type Draft struct {
	ID                   string              `json:"id"`
	Version              int64               `json:"version"`
	State                string              `json:"state"`
	Source               json.RawMessage     `json:"source"`
	ResolvedCommit       string              `json:"resolved_commit,omitempty"`
	ResolvedTree         string              `json:"resolved_tree,omitempty"`
	Manifest             json.RawMessage     `json:"manifest"`
	Candidates           []Candidate         `json:"candidates"`
	SelectedAssets       []AssetSelection    `json:"selected_assets"`
	Diagnostics          []Diagnostic        `json:"diagnostics"`
	Operation            *Operation          `json:"operation,omitempty"`
	Proposal             *Proposal           `json:"proposal,omitempty"`
	ContextSelection     []ContextFile       `json:"context_selection"`
	Questions            []Question          `json:"questions"`
	AcknowledgedWarnings []string            `json:"acknowledged_warnings"`
	ResolvedReferences   []ResolvedReference `json:"resolved_references"`
	ParentDraftID        string              `json:"parent_draft_id,omitempty"`
	ChangeContext        *ChangeContext      `json:"change_context,omitempty"`
	PackageDigest        string              `json:"package_digest,omitempty"`
	RunID                string              `json:"run_id,omitempty"`
	CreatedAt            string              `json:"created_at"`
	UpdatedAt            string              `json:"updated_at"`
}

// Answer is one answered proposal question supplied by the operator.
type Answer struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

// UpdateRequest is the operator edit contract.
type UpdateRequest struct {
	Manifest             json.RawMessage  `json:"manifest"`
	SelectedAssets       []AssetSelection `json:"selected_assets"`
	Answers              []Answer         `json:"answers,omitempty"`
	AcknowledgedWarnings *[]string        `json:"acknowledged_warnings,omitempty"`
}

// Error is a typed builder failure with corrective diagnostics.
type Error struct {
	Code        string
	Message     string
	Diagnostics []Diagnostic
	Retryable   bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// SortDiagnostics gives findings a stable, deterministic order.
func SortDiagnostics(diags []Diagnostic) {
	sort.Slice(diags, func(i, j int) bool { return diags[i].ID < diags[j].ID })
}

// nowStamp is the persisted operation timestamp format.
func nowStamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// marshalJSON returns canonical compact JSON for a value.
func marshalJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
