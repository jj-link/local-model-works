package recipebuilder

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/recipe"
	"os/exec"
)

const (
	maxCandidates = 10_000
	maxFileBytes  = 16 << 20
	maxTotalBytes = 128 << 20
)

type Candidate struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Binary bool   `json:"binary,omitempty"`
}

type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

type Draft struct {
	ID             string          `json:"id"`
	Version        int64           `json:"version"`
	State          string          `json:"state"`
	Source         json.RawMessage `json:"source"`
	ResolvedCommit string          `json:"resolved_commit,omitempty"`
	ResolvedTree   string          `json:"resolved_tree,omitempty"`
	Manifest       json.RawMessage `json:"manifest"`
	Candidates     []Candidate     `json:"candidates"`
	SelectedAssets []string        `json:"selected_assets"`
	Diagnostics    []Diagnostic    `json:"diagnostics"`
	PackageDigest  string          `json:"package_digest,omitempty"`
	RunID          string          `json:"run_id,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

type Service struct {
	q         *db.Queries
	root      string
	validator *recipe.Validator
	recipes   *recipe.Service
}

func New(q *db.Queries, stateRoot string, validator *recipe.Validator, recipes *recipe.Service) *Service {
	return &Service{q: q, root: filepath.Join(stateRoot, "drafts"), validator: validator, recipes: recipes}
}

type GitSource struct {
	Remote   string `json:"remote"`
	Revision string `json:"revision"`
	Path     string `json:"path,omitempty"`
}

func (s *Service) CreateFromGit(ctx context.Context, source GitSource) (*Draft, error) {
	if source.Remote == "" || source.Revision == "" {
		return nil, fmt.Errorf("remote and revision are required")
	}
	checkout, err := os.MkdirTemp("", "lmw-draft-git-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkout)
	if err := runGit(ctx, checkout, "clone", "--no-checkout", source.Remote, checkout); err != nil {
		return nil, err
	}
	if err := runGit(ctx, checkout, "checkout", "--quiet", source.Revision); err != nil {
		return nil, err
	}
	commit, err := gitOutput(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	tree, err := gitOutput(ctx, checkout, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	rel := filepath.Clean(source.Path)
	if rel == "." {
		rel = ""
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("source path escapes checkout")
	}
	root := filepath.Join(checkout, rel)
	return s.CreateFromDir(ctx, source, commit, tree, nil, root)
}
func (s *Service) CreateFromDir(ctx context.Context, source any, resolvedCommit, resolvedTree string, manifest json.RawMessage, sourceDir string) (*Draft, error) {
	draftID, _ := id.New()
	draftRoot := filepath.Join(s.root, draftID)
	candidateRoot := filepath.Join(draftRoot, "source")
	if err := os.MkdirAll(candidateRoot, 0o700); err != nil {
		return nil, err
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(draftRoot)
		}
	}()
	candidates, diagnostics, err := inspectAndCopy(sourceDir, candidateRoot)
	if err != nil {
		return nil, err
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	candidateJSON, _ := json.Marshal(candidates)
	diagnosticJSON, _ := json.Marshal(diagnostics)
	if len(manifest) == 0 {
		manifest = json.RawMessage(`{}`)
	}
	if err := s.q.CreateRecipeDraft(ctx, db.CreateRecipeDraftParams{
		ID: draftID, State: "needs_input", Source: string(sourceJSON),
		ResolvedCommit: nullable(resolvedCommit), ResolvedTree: nullable(resolvedTree),
		Manifest: string(manifest), Candidates: string(candidateJSON),
		SelectedAssets: "[]", Diagnostics: string(diagnosticJSON),
	}); err != nil {
		return nil, err
	}
	cleanup = false
	return s.Get(ctx, draftID)
}

func inspectAndCopy(sourceRoot, destination string) ([]Candidate, []Diagnostic, error) {
	var candidates []Candidate
	var diagnostics []Diagnostic
	var total int64
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			diagnostics = append(diagnostics, Diagnostic{Code: "recipe.draft_symlink", Severity: "warning", Path: filepath.ToSlash(rel), Message: "symlink inventoried but not copied"})
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if len(candidates) >= maxCandidates {
			return fmt.Errorf("draft candidate count exceeds %d", maxCandidates)
		}
		if info.Size() > maxFileBytes {
			diagnostics = append(diagnostics, Diagnostic{Code: "recipe.draft_oversize", Severity: "warning", Path: filepath.ToSlash(rel), Message: "file exceeds 16 MiB and was not copied"})
			return nil
		}
		total += info.Size()
		if total > maxTotalBytes {
			return fmt.Errorf("draft candidates exceed 128 MiB")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		prefix := make([]byte, 8192)
		read, _ := file.Read(prefix)
		prefix = prefix[:read]
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return err
		}
		stored := filepath.Join(destination, "sha256-"+hashFileName(path))
		output, err := os.OpenFile(stored, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
		if err != nil {
			file.Close()
			return err
		}
		_, err = io.Copy(io.MultiWriter(output, hash), file)
		file.Close()
		closeErr := output.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		final := filepath.Join(destination, "sha256-"+digest)
		if stored != final {
			if err := os.Rename(stored, final); err != nil {
				return err
			}
		}
		binary := bytesContainNUL(prefix)
		candidate := Candidate{Path: filepath.ToSlash(rel), Size: info.Size(), SHA256: digest, Binary: binary}
		candidates = append(candidates, candidate)
		if binary {
			diagnostics = append(diagnostics, Diagnostic{Code: "recipe.draft_binary", Severity: "warning", Path: candidate.Path, Message: "binary candidate requires explicit review"})
		}
		lower := strings.ToLower(string(prefix))
		if strings.Contains(lower, "docker run") || strings.Contains(lower, "--privileged") {
			diagnostics = append(diagnostics, Diagnostic{Code: "recipe.draft_host_lifecycle", Severity: "warning", Path: candidate.Path, Message: "host Docker lifecycle is incompatible and will never execute"})
		}
		return nil
	})
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, diagnostics, err
}

func hashFileName(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])
}

func bytesContainNUL(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

func (s *Service) Get(ctx context.Context, draftID string) (*Draft, error) {
	row, err := s.q.GetRecipeDraft(ctx, draftID)
	if err != nil {
		return nil, err
	}
	return render(row)
}

func (s *Service) List(ctx context.Context) ([]Draft, error) {
	rows, err := s.q.ListRecipeDrafts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Draft, 0, len(rows))
	for _, row := range rows {
		draft, err := render(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *draft)
	}
	return out, nil
}

func render(row db.RecipeDraft) (*Draft, error) {
	draft := &Draft{
		ID: row.ID, Version: row.Version, State: row.State, Source: json.RawMessage(row.Source),
		ResolvedCommit: value(row.ResolvedCommit), ResolvedTree: value(row.ResolvedTree),
		Manifest: json.RawMessage(row.Manifest), PackageDigest: value(row.PackageDigest),
		RunID: value(row.RunID), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if err := json.Unmarshal([]byte(row.Candidates), &draft.Candidates); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.SelectedAssets), &draft.SelectedAssets); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.Diagnostics), &draft.Diagnostics); err != nil {
		return nil, err
	}
	return draft, nil
}

func (s *Service) Update(ctx context.Context, draftID string, version int64, manifest json.RawMessage, selectedAssets []string) (*Draft, error) {
	row, err := s.q.GetRecipeDraft(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.State == "analyzing" {
		return nil, fmt.Errorf("recipe.draft_busy")
	}
	var candidates []Candidate
	if err := json.Unmarshal([]byte(row.Candidates), &candidates); err != nil {
		return nil, err
	}
	available := map[string]bool{}
	for _, candidate := range candidates {
		available[candidate.SHA256] = true
	}
	for _, selected := range selectedAssets {
		if !available[selected] {
			return nil, fmt.Errorf("recipe.draft_source_changed: selected hash %s is unavailable", selected)
		}
	}
	diagnostics := []Diagnostic{}
	validation, err := s.validator.Validate(manifest)
	if err != nil {
		return nil, err
	}
	for _, finding := range validation {
		diagnostics = append(diagnostics, Diagnostic{Code: finding.Code, Severity: finding.Severity, Path: finding.Path, Message: finding.Message})
	}
	state := "valid"
	if len(diagnostics) > 0 {
		state = "needs_input"
	}
	selectedJSON, _ := json.Marshal(selectedAssets)
	diagnosticsJSON, _ := json.Marshal(diagnostics)
	rows, err := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: state, Manifest: string(manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), ID: draftID, Version: version,
	})
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, fmt.Errorf("recipe.draft_version_conflict")
	}
	return s.Get(ctx, draftID)
}

func (s *Service) Package(ctx context.Context, draftID string) (*Draft, error) {
	draft, err := s.Get(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if draft.State != "valid" {
		return nil, fmt.Errorf("recipe draft must be valid before packaging")
	}
	selected := map[string]bool{}
	for _, hash := range draft.SelectedAssets {
		selected[hash] = true
	}
	assets := map[string][]byte{}
	for _, candidate := range draft.Candidates {
		if !selected[candidate.SHA256] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+candidate.SHA256))
		if err != nil {
			return nil, fmt.Errorf("recipe.draft_source_changed: %w", err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != candidate.SHA256 {
			return nil, fmt.Errorf("recipe.draft_source_changed: %s", candidate.Path)
		}
		assets[candidate.Path] = data
	}
	parsed, diagnostics, err := s.validator.ValidateStrict(draft.Manifest)
	if err != nil {
		return nil, err
	}
	if len(diagnostics) != 0 {
		return nil, fmt.Errorf("recipe draft is not valid")
	}
	for _, asset := range parsed.Assets {
		if _, ok := assets[asset]; !ok {
			return nil, fmt.Errorf("recipe.draft_source_changed: selected asset %s is unavailable", asset)
		}
	}
	if len(assets) != len(parsed.Assets) {
		return nil, fmt.Errorf("selected assets must exactly match manifest assets")
	}
	packed, err := recipe.PackManifest(draft.Manifest, assets, nil)
	if err != nil {
		return nil, err
	}
	layout := filepath.Join(s.root, draftID, "package")
	_ = recipe.RemovePackage(layout)
	if err := recipe.WriteLayout(layout, packed); err != nil {
		return nil, err
	}
	selectedJSON, _ := json.Marshal(draft.SelectedAssets)
	diagnosticsJSON, _ := json.Marshal(draft.Diagnostics)
	rows, err := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: "packaged", Manifest: string(draft.Manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), PackageDigest: nullable(packed.ManifestDigest),
		ID: draftID, Version: draft.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("recipe draft package update failed: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("recipe.draft_version_conflict")
	}
	return s.Get(ctx, draftID)
}

func (s *Service) Install(ctx context.Context, draftID string, permissionDiffAccepted bool) (*recipe.Recipe, error) {
	if !permissionDiffAccepted {
		return nil, fmt.Errorf("permission diff acceptance is required")
	}
	draft, err := s.Get(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if draft.State == "analyzing" && draft.PackageDigest != "" && s.recipes != nil {
		detail, lookupErr := s.recipes.Get(ctx, draft.PackageDigest)
		if lookupErr == nil {
			selectedJSON, _ := json.Marshal(draft.SelectedAssets)
			diagnosticsJSON, _ := json.Marshal(draft.Diagnostics)
			rows, updateErr := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
				State: "installed", Manifest: string(draft.Manifest), SelectedAssets: string(selectedJSON),
				Diagnostics: string(diagnosticsJSON), PackageDigest: nullable(detail.Digest),
				ID: draftID, Version: draft.Version,
			})
			if updateErr != nil || rows != 1 {
				return nil, fmt.Errorf("recipe draft install recovery failed")
			}
			return &detail.Recipe, nil
		}
	}
	if draft.State != "packaged" || draft.PackageDigest == "" || s.recipes == nil {
		return nil, fmt.Errorf("recipe draft is not packaged")
	}
	selectedJSON, _ := json.Marshal(draft.SelectedAssets)
	diagnosticsJSON, _ := json.Marshal(draft.Diagnostics)
	rows, err := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: "analyzing", Manifest: string(draft.Manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), PackageDigest: nullable(draft.PackageDigest),
		ID: draftID, Version: draft.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("recipe draft install reservation failed: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("recipe.draft_version_conflict")
	}
	installed, err := s.recipes.Import(ctx, recipe.RecipeSource{
		Type: "local", Path: filepath.Join(s.root, draftID, "package"),
	})
	if err != nil {
		_, _ = s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
			State: "packaged", Manifest: string(draft.Manifest), SelectedAssets: string(selectedJSON),
			Diagnostics: string(diagnosticsJSON), PackageDigest: nullable(draft.PackageDigest),
			ID: draftID, Version: draft.Version + 1,
		})
		return nil, err
	}
	rows, err = s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: "installed", Manifest: string(draft.Manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), PackageDigest: nullable(installed.Digest),
		ID: draftID, Version: draft.Version + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("recipe draft install update failed: %w", err)
	}
	if rows != 1 {
		return nil, fmt.Errorf("recipe.draft_install_state_lost")
	}
	_ = os.RemoveAll(filepath.Join(s.root, draftID, "source"))
	return &installed, nil
}

func (s *Service) Delete(ctx context.Context, draftID string) error {
	row, err := s.q.GetRecipeDraft(ctx, draftID)
	if err != nil {
		return err
	}
	if row.State == "analyzing" {
		return fmt.Errorf("recipe.draft_busy")
	}
	if err := s.q.DeleteRecipeDraft(ctx, draftID); err != nil {
		return err
	}
	root := filepath.Join(s.root, draftID)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

func runGit(ctx context.Context, dir string, args ...string) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func nullable(value string) sql.NullString { return sql.NullString{String: value, Valid: value != ""} }
func value(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
