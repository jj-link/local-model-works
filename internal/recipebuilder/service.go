// Package recipebuilder persists and inspects recipe draft work: it
// allocates a draft for a source GitHub repository, reserves
// version-owned operations (inspect, generate, resolve, package,
// install), and applies the CAS-guarded terminal state each operation
// promises. Draft source, pin, manifest, candidates, and diagnostics
// survive failure and restart, so every operation is resumable or
// retryable from a persistent draft.
package recipebuilder

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

const (
	maxCandidates   = 10_000
	maxFileBytes    = 16 << 20
	maxTotalBytes   = 128 << 20
	cloneBudget     = 512 << 20
	inspectTimeout  = 5 * time.Minute
	revisionLimit   = 256
	maxContextFile  = 64 << 10
	maxContextTotal = 256 << 10
	gitOutputLimit  = 64 << 10
	defaultManifest = `{}`
)

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
)

// GitSource is the operator-supplied repository source.
type GitSource struct {
	Remote   string `json:"remote"`
	Revision string `json:"revision"`
	Path     string `json:"path,omitempty"`
}

// Service is the draft workflow engine.
type Service struct {
	q         *db.Queries
	db        *sql.DB
	root      string
	validator *recipe.Validator
	recipes   *recipe.Service
	registry  recipe.RepositoryCompilerRegistry
}

// New builds a draft service over the shared state root.
func New(q *db.Queries, stateRoot string, validator *recipe.Validator, recipes *recipe.Service) *Service {
	return &Service{q: q, root: filepath.Join(stateRoot, "drafts"), validator: validator, recipes: recipes}
}

// SetDB exposes the shared handle for multi-row transactional work.
func (s *Service) SetDB(database *sql.DB) { s.db = database }

// SetRepositoryCompilerRegistry installs the deterministic compiler registry.
func (s *Service) SetRepositoryCompilerRegistry(registry recipe.RepositoryCompilerRegistry) {
	s.registry = registry
}

// Allocate persists a draft for an unverified source. It performs no network
// or git work: source reachability and revision resolution belong to the
// reserved inspect operation.
func (s *Service) Allocate(ctx context.Context, source GitSource) (*Draft, error) {
	remote := strings.TrimSpace(source.Remote)
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, newError("recipe.source_invalid",
			"remote must be an https://github.com/owner/repo URL without credentials, query, or fragment", false)
	}
	segments := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, "/"), "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return nil, newError("recipe.source_invalid", "remote must be exactly https://github.com/owner/repo", false)
	}
	if !validRevision(source.Revision) {
		return nil, newError("recipe.source_invalid",
			"revision must be empty for the default branch, a branch or tag name, or a 40-hex commit", false)
	}
	source.Revision = strings.TrimSpace(source.Revision)
	if commitRE.MatchString(strings.ToLower(source.Revision)) {
		source.Revision = strings.ToLower(source.Revision)
	}
	_, normalizedRemote, normalizedPath, err := recipe.RepositoryIdentity(recipe.Source{
		URL: remote, Path: source.Path,
	})
	if err != nil {
		return nil, newError("recipe.source_invalid", err.Error(), false)
	}
	source.Remote = normalizedRemote
	if normalizedPath == "." {
		normalizedPath = ""
	}
	source.Path = normalizedPath
	if source.Path != "" {
		clean := filepath.Clean(filepath.FromSlash(source.Path))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, newError("recipe.source_invalid", "source path must stay inside the repository", false)
		}
		source.Path = filepath.ToSlash(clean)
	}
	draftID, err := id.New()
	if err != nil {
		return nil, err
	}
	sourceJSON, err := marshalJSON(source)
	if err != nil {
		return nil, err
	}
	if err := s.q.CreateRecipeDraft(ctx, db.CreateRecipeDraftParams{
		ID: draftID, State: "needs_input", Source: sourceJSON,
		Manifest: defaultManifest, Candidates: "[]", SelectedAssets: "[]", Diagnostics: "[]",
		ContextSelection: "[]", Questions: "[]", AcknowledgedWarnings: "[]",
		ResolvedReferences: "[]", ChangeContext: nullable(`{"kind":"add","base_source_status":"unavailable"}`),
	}); err != nil {
		return nil, err
	}
	return s.Get(ctx, draftID)
}

func validRevision(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	if commitRE.MatchString(strings.ToLower(ref)) {
		return true
	}
	return len(ref) <= revisionLimit && refRE.MatchString(ref)
}

// getDraftRow returns the raw row, mapping missing rows to a typed 404 error.
func (s *Service) getDraftRow(ctx context.Context, draftID string) (db.RecipeDraft, error) {
	row, err := s.q.GetRecipeDraft(ctx, draftID)
	if errors.Is(err, sql.ErrNoRows) {
		return row, newError("recipe.draft_unknown", "draft "+draftID+" does not exist", false)
	}
	return row, err
}

// requireOperation validates that the row's active operation matches
// operationID and, when kind is nonempty, its kind.
func requireOperation(row db.RecipeDraft, operationID, kind string) (*Operation, error) {
	if !row.Operation.Valid {
		return nil, newError("recipe.draft_operation_conflict", "draft has no active operation", false)
	}
	var op Operation
	if err := json.Unmarshal([]byte(row.Operation.String), &op); err != nil || op.ID == "" {
		return nil, newError("recipe.draft_operation_conflict", "draft operation record is corrupt", false)
	}
	if op.ID != operationID {
		return nil, newError("recipe.draft_operation_conflict", "operation does not own this draft", false)
	}
	if kind != "" && op.Kind != kind {
		return nil, newError("recipe.draft_operation_conflict", "draft is not reserved for this operation kind", false)
	}
	return &op, nil
}

// ReserveOperation is the only If-Match-to-operation conversion: it bumps the
// version once, records the owning operation UUID and previous state, and
// moves the draft to analyzing. Every other mutation is rejected while an
// operation is active.
func (s *Service) ReserveOperation(ctx context.Context, draftID string, version int64, kind string) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	switch kind {
	case PhaseInspect:
		var candidates []Candidate
		_ = json.Unmarshal([]byte(row.Candidates), &candidates)
		var change ChangeContext
		_ = json.Unmarshal([]byte(value(row.ChangeContext)), &change)
		initialChange := change.BaseRecipeDigest != "" && !row.RunID.Valid
		missingRepairSource := change.Kind == "repair" && change.BaseSourceStatus == "unavailable" && row.State != "installed" && row.State != "packaged"
		if !initialChange && !missingRepairSource && row.State != "failed" && !(row.State == "needs_input" && len(candidates) == 0) {
			return nil, newError("recipe.draft_operation_conflict",
				"inspect is only allowed before an inventory exists or after a failed inspection", false)
		}
	case PhaseGenerate, PhaseResolve:
		if row.State != "needs_input" && row.State != "valid" && row.State != "failed" {
			return nil, newError("recipe.draft_operation_conflict", "generation requires an editable draft", false)
		}
	case PhasePackage:
		if row.State != "valid" {
			return nil, newError("recipe.draft_operation_conflict", "only validated drafts can be packaged", false)
		}
	case PhaseInstall:
		if row.State != "packaged" && row.State != "installed" {
			return nil, newError("recipe.draft_operation_conflict", "installation requires a packaged draft", false)
		}
	default:
		return nil, newError("recipe.draft_operation_conflict", "unknown operation kind: "+kind, false)
	}
	opID, err := id.New()
	if err != nil {
		return nil, err
	}
	now := nowStamp()
	op := Operation{
		ID: opID, Kind: kind, PreviousState: row.State,
		Phase: "reserved", StartedAt: now, UpdatedAt: now,
	}
	opJSON, err := marshalJSON(op)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ReserveRecipeDraftOperation(ctx, db.ReserveRecipeDraftOperationParams{
		Operation: nullable(opJSON), ID: draftID, Version: version,
	})
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	return s.Get(ctx, draftID)
}

// attachRun links the owning run to the active operation.
func (s *Service) attachRun(ctx context.Context, draftID, operationID, runID string) error {
	rows, err := s.q.AttachRecipeDraftOperationRun(ctx, db.AttachRecipeDraftOperationRunParams{
		RunID: nullable(runID), ID: draftID, Operation: nullable(operationID),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return newError("recipe.draft_operation_lost", "the active operation changed before the run was attached", false)
	}
	return nil
}

// ReportOperationProgress updates the active operation's phase and message
// without bumping the draft version.
func (s *Service) ReportOperationProgress(ctx context.Context, draftID, operationID, phase, message string) error {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return err
	}
	op, err := requireOperation(row, operationID, "")
	if err != nil {
		return err
	}
	op.Phase = phase
	op.Message = message
	op.UpdatedAt = nowStamp()
	opJSON, err := marshalJSON(op)
	if err != nil {
		return err
	}
	rows, err := s.q.UpdateRecipeDraftOperation(ctx, db.UpdateRecipeDraftOperationParams{
		Operation: nullable(opJSON), ID: draftID, Operation_2: nullable(operationID),
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return newError("recipe.draft_operation_lost", "the active operation changed before the update", false)
	}
	return nil
}

// FailOperation completes an active operation as a failure: inspect failures
// land in failed; every other kind returns to the state the operation was
// reserved from. The cause is appended as a dismissible, retryable-aware
// diagnostic with an operation-scoped ID.
func (s *Service) FailOperation(ctx context.Context, draftID, operationID string, cause error) (*Draft, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, "")
	if err != nil {
		return nil, err
	}
	return s.failOperation(ctx, row, op, nil, nil, nil, cause)
}

// failOperation applies the failure terminal state. Nil candidates,
// selectedAssets, or diagnostics fall back to the row's persisted values.
func (s *Service) failOperation(ctx context.Context, row db.RecipeDraft, op *Operation,
	candidates []Candidate, selected []AssetSelection, diags []Diagnostic, cause error) (*Draft, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if latest, err := s.getDraftRow(ctx, row.ID); err == nil {
		if latestOp, opErr := requireOperation(latest, op.ID, op.Kind); opErr == nil {
			row = latest
			op = latestOp
		}
	}
	state := op.PreviousState
	if op.Kind == PhaseInspect {
		state = "failed"
	}
	var causeInfo *Error
	if cause != nil {
		errors.As(cause, &causeInfo)
	}
	code, message, retryable := "recipe.draft_operation_failed", "operation failed", true
	if causeInfo != nil {
		code, message, retryable = causeInfo.Code, causeInfo.Message, causeInfo.Retryable
	}
	var providerError *recipeassistant.Error
	if errors.As(cause, &providerError) {
		code, message, retryable = providerError.Code, providerError.Message, providerError.Retryable
	}
	if errors.Is(cause, context.Canceled) {
		code, message, retryable = "recipe.operation_cancelled", "operation was cancelled", true
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		code, message, retryable = "recipe.operation_timeout", "operation exceeded its time limit", true
	}
	if diags == nil {
		if err := json.Unmarshal([]byte(row.Diagnostics), &diags); err != nil {
			diags = []Diagnostic{}
		}
	}
	attempt := Diagnostic{
		Code: code, Severity: "error", Message: message, Phase: op.Kind,
		Blocking: true, Retryable: retryable, Dismissible: true,
	}
	attempt.ID = DiagnosticID(attempt, op.ID)
	diags = append(diags, attempt)
	if _, err := s.completeOperation(ctx, row, op, state, json.RawMessage{}, candidates, selected, diags,
		value(row.PackageDigest), value(row.RunID)); err != nil {
		return nil, err
	}
	if cause != nil {
		return nil, cause
	}
	return s.Get(ctx, row.ID)
}

// Inspect executes a reserved inspect operation: resolve and pin the source
// revision, clone it in a service-owned checkout, verify the checkout against
// the pin, inventory the files, and compile a starter manifest when a
// recognized compiler matches the source. Success completes the operation to
// needs_input (plain inventory) or valid (recognized clean manifest);
// failure completes it to failed while preserving the source, pin, and
// evidence gathered so far.
func (s *Service) Inspect(ctx context.Context, draftID, operationID, runID string,
	progress func(phase, message string)) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.ChangeContext.Valid {
		var change ChangeContext
		if json.Unmarshal([]byte(row.ChangeContext.String), &change) == nil && change.BaseRecipeDigest != "" {
			return s.prepareChange(ctx, draftID, operationID, runID, progress, true)
		}
	}
	op, err := requireOperation(row, operationID, PhaseInspect)
	if err != nil {
		return nil, err
	}
	if row.State != "analyzing" {
		return nil, newError("recipe.draft_operation_conflict", "draft is not reserved for inspection", false)
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}

	var source GitSource
	if err := json.Unmarshal([]byte(row.Source), &source); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil,
			newError("recipe.source_invalid", "draft source is not parseable", false))
	}
	report := func(phase, message string) {
		if progress != nil {
			progress(phase, message)
		}
	}

	report(PhaseInspect, "resolving source revision")
	commit, tree, inspectDir, err := s.checkoutPinned(ctx, draftID, op.ID, source, value(row.ResolvedCommit))
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	checkoutDir := filepath.Join(s.root, draftID, "inspection", op.ID, "checkout")
	row, err = s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err = requireOperation(row, operationID, PhaseInspect)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Join(s.root, draftID, "inspection"))

	report(PhaseInspect, "inventorying source files")
	candidates, exclusions, findings, err := s.ingestAndCopy(inspectDir, filepath.Join(s.root, draftID, "source"))
	if err != nil {
		return s.failOperation(ctx, row, op, candidates, nil, findings, err)
	}
	if err := writeExclusions(filepath.Join(s.root, draftID, "exclusions.json"), exclusions); err != nil {
		return s.failOperation(ctx, row, op, candidates, nil, findings,
			newError("recipe.draft_io", "cannot persist context exclusions: "+err.Error(), true))
	}

	manifest := json.RawMessage(row.Manifest)
	recognized := false
	report(PhaseInspect, "preparing draft")
	if s.registry != nil {
		repoSource := recipe.RepositorySource{
			RepositoryID: repositoryID(source), URL: source.Remote, Path: source.Path,
			CommitSHA: commit, TreeSHA: tree,
		}
		if compiler, found := s.registry.Lookup(repoSource, checkoutDir); found {
			packed, compileErr := compiler.Compile(ctx, repoSource, checkoutDir, nil)
			if compileErr != nil {
				diag := Diagnostic{
					Code: "recipe.compile_failed", Severity: "warning",
					Message: "recognized compiler failed; the draft keeps the source inventory: " + compileErr.Error(),
					Phase:   PhaseInspect, Retryable: true,
				}
				diag.ID = DiagnosticID(diag, op.ID)
				findings = append(findings, diag)
			} else {
				manifest = packed.ConfigJSON
				recognized = true
				generated, genErr := s.importCompiledAssets(draftID, packed, op.ID)
				if genErr != nil {
					return s.failOperation(ctx, row, op, candidates, nil, findings, genErr)
				}
				candidates = append(candidates, generated...)
				sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
			}
		}
	}
	manifest, err = pinnedManifest(row, manifest)
	if err != nil {
		return s.failOperation(ctx, row, op, candidates, nil, findings, err)
	}
	validateFindings := s.validatorFindings(manifest)
	findings = append(findings, validateFindings...)
	state := "needs_input"
	var selected []AssetSelection
	if recognized {
		var assetFindings []Diagnostic
		selected, assetFindings = selectManifestAssets(manifest, candidates)
		findings = append(findings, assetFindings...)
		if allClean(validateFindings) && !anyAssetMissing(assetFindings) {
			state = "valid"
		}
	}
	SortDiagnostics(findings)
	return s.completeOperation(ctx, row, op, state, manifest, candidates,
		selected, findings, value(row.PackageDigest), runID)
}

// checkoutPinned resolves and pins the revision, clones the repository in a
// service-owned checkout, and verifies the checkout against the pin.
func (s *Service) checkoutPinned(ctx context.Context, draftID, opID string,
	source GitSource, pinnedCommit string) (commit, tree, inspectDir string, err error) {
	remote := strings.TrimSuffix(source.Remote, "/")
	revision := strings.TrimSpace(source.Revision)
	if commitRE.MatchString(strings.ToLower(revision)) {
		revision = strings.ToLower(revision)
	}
	commit = pinnedCommit
	if commit == "" {
		commit, err = resolveRevision(ctx, remote, revision)
		if err != nil {
			return "", "", "", err
		}
		rows, pinErr := s.q.PinRecipeDraftSource(ctx, db.PinRecipeDraftSourceParams{
			ResolvedCommit: nullable(commit), ResolvedTree: sql.NullString{},
			ID: draftID, Operation: nullable(opID), ResolvedCommit_2: nullable(commit),
		})
		if pinErr != nil {
			return "", "", "", pinErr
		}
		if rows != 1 {
			return "", "", "", newError("recipe.source_pin_moved", "the draft source pin changed concurrently", false)
		}
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, inspectTimeout)
	defer timeoutCancel()
	inspectDir = filepath.Join(s.root, draftID, "inspection", opID, "checkout")
	if err := os.MkdirAll(filepath.Dir(inspectDir), 0o700); err != nil {
		return "", "", "", newError("recipe.draft_io", err.Error(), true)
	}
	cloneCtx, budgetExceeded, stopBudgetWatch := withDirectoryBudget(timeoutCtx, inspectDir, cloneBudget)
	defer stopBudgetWatch()
	if err := runGitEnv(cloneCtx, "", "clone", "--filter=blob:none", "--no-checkout", "--quiet", remote, inspectDir); err != nil {
		if budgetExceeded() {
			return commit, "", "", newError("recipe.source_too_large", "checkout exceeded the 512 MiB inspection budget", false)
		}
		return commit, "", "", classifyCloneError(timeoutCtx, err)
	}
	if err := runGitEnv(cloneCtx, inspectDir, "checkout", "--quiet", commit); err != nil {
		if budgetExceeded() {
			return commit, "", "", newError("recipe.source_too_large", "checkout exceeded the 512 MiB inspection budget", false)
		}
		return commit, "", "", newError("recipe.source_checkout_failed", "git checkout of pinned commit failed: "+err.Error(), true)
	}
	head, err := gitOutput(ctx, inspectDir, "rev-parse", "HEAD")
	if err != nil {
		return commit, "", "", newError("recipe.source_checkout_failed", "cannot read checkout head: "+err.Error(), true)
	}
	if head != commit {
		// A pinned tag object can resolve to its commit on checkout.
		pinnedCommit, pinErr := gitOutput(ctx, inspectDir, "rev-parse", commit+"^{commit}")
		if pinErr != nil || pinnedCommit != head {
			return commit, "", "", newError("recipe.source_pin_mismatch", "checkout head differs from the pinned commit", false)
		}
	}
	tree, err = gitOutput(ctx, inspectDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return commit, "", "", newError("recipe.source_checkout_failed", "cannot read checkout tree: "+err.Error(), true)
	}
	rows, pinErr := s.q.PinRecipeDraftSource(ctx, db.PinRecipeDraftSourceParams{
		ResolvedCommit: nullable(commit), ResolvedTree: nullable(tree),
		ID: draftID, Operation: nullable(opID), ResolvedCommit_2: nullable(commit),
	})
	if pinErr != nil {
		return commit, tree, "", pinErr
	}
	if rows != 1 {
		return commit, tree, "", newError("recipe.source_pin_moved", "the draft source pin changed concurrently", false)
	}
	size, err := treeSize(ctx, inspectDir)
	if err != nil {
		return commit, tree, "", newError("recipe.draft_io", "cannot bound checkout size: "+err.Error(), true)
	}
	if size > cloneBudget {
		return commit, tree, "", newError("recipe.source_too_large",
			fmt.Sprintf("checkout is %d bytes; the 512 MiB inspection budget is exceeded", size), false)
	}
	root, err := safeSubpath(inspectDir, source.Path)
	if err != nil {
		return commit, tree, "", newError("recipe.source_subpath_invalid", err.Error(), false)
	}
	return commit, tree, root, nil
}

// resolveRevision resolves an operator revision to a full commit SHA. The
// result is pinned before any checkout, so retries reuse the same commit.
func resolveRevision(ctx context.Context, remote, revision string) (string, error) {
	if revision == "" {
		out, err := gitOutput(ctx, "", "ls-remote", remote, "HEAD")
		if err != nil {
			return "", newError("recipe.source_unreachable", "git ls-remote failed: "+err.Error(), true)
		}
		fields := strings.Fields(out)
		if len(fields) < 2 || !commitRE.MatchString(fields[0]) {
			return "", newError("recipe.source_ref_unresolved", "cannot resolve the default branch", false)
		}
		return fields[0], nil
	}
	if commitRE.MatchString(revision) {
		return revision, nil
	}
	out, err := gitOutput(ctx, "", "ls-remote", remote, "refs/heads/"+revision, "refs/tags/"+revision, "refs/tags/"+revision+"^{}")
	if err != nil {
		return "", newError("recipe.source_unreachable", "git ls-remote failed: "+err.Error(), true)
	}
	head, tag, peeled, saw := "", "", "", map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !commitRE.MatchString(fields[0]) {
			continue
		}
		ref := fields[1]
		switch {
		case ref == "refs/heads/"+revision:
			head = fields[0]
			saw["head"] = true
		case ref == "refs/tags/"+revision:
			tag = fields[0]
			saw["tag"] = true
		case ref == "refs/tags/"+revision+"^{}":
			peeled = fields[0]
		}
	}
	if saw["head"] && saw["tag"] {
		return "", newError("recipe.source_ref_ambiguous", "the revision names both a branch and a tag", false)
	}
	switch {
	case saw["head"]:
		return head, nil
	case saw["tag"]:
		if peeled != "" {
			return peeled, nil
		}
		return tag, nil
	default:
		return "", newError("recipe.source_ref_unresolved", "the revision is not a branch or tag on the remote", false)
	}
}

func classifyCloneError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return newError("recipe.source_timeout", "git clone exceeded the inspection timeout", true)
	}
	return newError("recipe.source_clone_failed", "git clone failed: "+err.Error(), true)
}

// safeSubpath resolves a relative subpath component-by-component against the
// checkout root, rejecting symlink components and paths that escape.
func safeSubpath(root, rel string) (string, error) {
	rel = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))), "/")
	if rel == "." || rel == "" {
		return root, nil
	}
	cur := root
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." || comp == ".." {
			return "", fmt.Errorf("invalid subpath component")
		}
		cur = filepath.Join(cur, comp)
		info, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("subpath component is a symlink")
		}
	}
	return cur, nil
}

// ingestAndCopy inventories regular files under srcRoot into the
// content-addressed candidate store. It returns candidates, context
// exclusions (data that must never reach the assistant context), and
// inspection findings. Partial results are returned alongside errors so a
// failed inspection can preserve the evidence already gathered.
func (s *Service) ingestAndCopy(srcRoot, destination string) ([]Candidate, []ContextExclusion, []Diagnostic, error) {
	var candidates []Candidate
	var exclusions []ContextExclusion
	var findings []Diagnostic
	var total int64
	err := filepath.WalkDir(srcRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		base := filepath.Base(rel)
		if entry.IsDir() {
			if reason := excludedDir(rel); reason != "" {
				exclusions = append(exclusions, ContextExclusion{Path: rel, Reason: reason, SizeBytes: 0})
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			exclusions = append(exclusions, ContextExclusion{Path: rel, Reason: "symlink", SizeBytes: 0})
			findings = append(findings, inspectFinding("recipe.draft_symlink", "symlink inventoried but not copied", rel))
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if reason := excludedFile(base); reason != "" {
			exclusions = append(exclusions, ContextExclusion{Path: rel, Reason: reason, SizeBytes: info.Size()})
			return nil
		}
		if len(candidates) >= maxCandidates {
			return fmt.Errorf("draft candidate count exceeds %d", maxCandidates)
		}
		if info.Size() > maxFileBytes {
			exclusions = append(exclusions, ContextExclusion{Path: rel, Reason: "oversize", SizeBytes: info.Size()})
			findings = append(findings, inspectFinding("recipe.draft_oversize", "file exceeds 16 MiB and was not copied", rel))
			return nil
		}
		total += info.Size()
		if total > maxTotalBytes {
			return fmt.Errorf("draft candidates exceed 128 MiB")
		}
		digest, binary, err := storeCandidate(path, destination)
		if err != nil {
			return err
		}
		candidates = append(candidates, Candidate{
			Path: rel, Size: info.Size(), SHA256: digest, Binary: binary,
			Origin: OriginSource, SourcePath: rel,
		})
		if binary {
			findings = append(findings, inspectFinding("recipe.draft_binary", "binary candidate requires explicit review", rel))
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		prefix, _ := io.ReadAll(io.LimitReader(file, 8192))
		file.Close()
		lower := strings.ToLower(string(prefix))
		if strings.Contains(lower, "docker run") || strings.Contains(lower, "--privileged") {
			findings = append(findings, inspectFinding("recipe.draft_host_lifecycle",
				"host Docker lifecycle is incompatible and will never execute", rel))
		}
		return nil
	})
	if err != nil {
		return candidates, exclusions, findings, newError("recipe.draft_inspection", err.Error(), false)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, exclusions, findings, nil
}

// excludedDir reports whether a directory (by repository path) is tool-local
// configuration or metadata that must stay out of the candidate store.
func excludedDir(rel string) string {
	base := strings.SplitN(rel, "/", 2)[0]
	switch base {
	case ".git":
		return "dot_git"
	case ".codex", ".agent", ".claude", ".cursor", ".github", ".idea", ".devcontainer", ".vscode":
		return "tool_config"
	default:
		return ""
	}
}

// excludedFile reports whether a file is secret material or environment data
// that must stay out of the candidate store. .env.sample and .env.example
// remain eligible.
func excludedFile(base string) string {
	lower := strings.ToLower(base)
	if lower == ".env" || strings.HasPrefix(lower, ".env.") {
		if lower == ".env.sample" || lower == ".env.example" {
			return ""
		}
		return "env_file"
	}
	credential := map[string]bool{
		"id_rsa": true, "id_ecdsa": true, "id_ed25519": true,
		".npmrc": true, ".pypirc": true, ".netrc": true,
		"credentials.json": true, "service-account.json": true,
		"docker-credential-desktop": true,
	}
	if credential[lower] {
		return "credential_file"
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(lower, suffix) {
			return "credential_file"
		}
	}
	return ""
}

func inspectFinding(code, message, rel string) Diagnostic {
	d := Diagnostic{
		Code: code, Severity: "warning", Path: rel, Message: message, Phase: PhaseInspect,
	}
	d.ID = DiagnosticID(d, "")
	return d
}

// storeCandidate hashes the source file and stores it content-addressed in
// the candidate store. An existing identical blob is reused, so interrupted
// inspections can be re-run without failing on already-copied files.
func storeCandidate(path, destination string) (string, bool, error) {
	digest, binary, err := hashFile(path)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", false, err
	}
	final := filepath.Join(destination, "sha256-"+digest)
	if _, err := os.Lstat(final); err == nil {
		return digest, binary, nil
	}
	tmp := filepath.Join(destination, "sha256-tmp-"+hashFileName(path))
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return "", false, err
	}
	file, err := os.Open(path)
	if err != nil {
		out.Close()
		return "", false, err
	}
	_, copyErr := io.Copy(out, file)
	file.Close()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", false, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", false, closeErr
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	return digest, binary, nil
}

// hashFile returns the content digest and binary classification of a file.
func hashFile(path string) (string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	hash := sha256.New()
	prefix := make([]byte, 8192)
	n, _ := file.Read(prefix)
	prefix = prefix[:n]
	binary := containsNUL(prefix)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", false, err
	}
	if _, err := io.Copy(hash, file); err != nil {
		return "", false, err
	}
	return hex.EncodeToString(hash.Sum(nil)), binary, nil
}

func hashFileName(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(filepath.ToSlash(path))))
	return hex.EncodeToString(sum[:])
}

func containsNUL(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

// importCompiledAssets unpacks a recognized compiler's verified asset layer
// into the draft candidate store as generated candidates.
func (s *Service) importCompiledAssets(draftID string, packed *recipe.PackResult, opID string) ([]Candidate, error) {
	draftRoot := filepath.Join(s.root, draftID)
	staging := filepath.Join(draftRoot, "staging", opID+"-compile")
	_ = recipe.RemovePackage(staging)
	if err := recipe.WriteLayout(staging, packed); err != nil {
		return nil, newError("recipe.draft_io", "cannot write compiler staging layout: "+err.Error(), true)
	}
	defer os.RemoveAll(staging)
	layerPath := filepath.Join(staging, "blobs", "sha256", strings.TrimPrefix(packed.LayerDigest, "sha256:"))
	layer, err := os.ReadFile(layerPath)
	if err != nil {
		return nil, newError("recipe.draft_io", "cannot read generated asset layer: "+err.Error(), true)
	}
	extract := staging + "-extract"
	if err := os.MkdirAll(extract, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(extract)
	if err := recipe.UnpackLayer(layer, extract); err != nil {
		return nil, newError("recipe.draft_io", "cannot unpack generated asset layer: "+err.Error(), true)
	}
	var candidates []Candidate
	err = filepath.WalkDir(extract, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(extract, path)
		if err != nil {
			return err
		}
		if rel == "." || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		digest, _, err := storeCandidate(path, filepath.Join(draftRoot, "source"))
		if err != nil {
			return err
		}
		candidates = append(candidates, Candidate{
			Path: filepath.ToSlash(rel), Size: info.Size(), SHA256: digest,
			Origin: OriginGenerated, SourcePath: "generated-layer",
		})
		return nil
	})
	if err != nil {
		return nil, newError("recipe.draft_io", "cannot store generated assets: "+err.Error(), true)
	}
	return candidates, nil
}

// writeExclusions persists the context exclusion sidecar for the draft.
func writeExclusions(path string, exclusions []ContextExclusion) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := marshalJSON(exclusions)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// Get returns one draft.
func (s *Service) Get(ctx context.Context, draftID string) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	return render(row)
}

// List returns every draft, most recently updated first.
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

// ListByPackageDigest lists drafts that produced the given package digest.
func (s *Service) ListByPackageDigest(ctx context.Context, digest string) ([]Draft, error) {
	rows, err := s.q.ListRecipeDraftsByPackageDigest(ctx, nullable(digest))
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

// ListByRepository returns recoverable work with a verified repository link.
func (s *Service) ListByRepository(ctx context.Context, repositoryID string) ([]Draft, error) {
	rows, err := s.q.ListRecipeDraftsByRepository(ctx, nullable(repositoryID))
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
		ID: row.ID, Version: row.Version, State: row.State,
		Source:         json.RawMessage(row.Source),
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
	for i := range draft.Diagnostics {
		if draft.Diagnostics[i].Severity == "warning" {
			draft.Diagnostics[i].Acknowledgement = warningHash(draft.Diagnostics[i])
		}
	}
	if err := json.Unmarshal([]byte(row.ContextSelection), &draft.ContextSelection); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.Questions), &draft.Questions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.AcknowledgedWarnings), &draft.AcknowledgedWarnings); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.ResolvedReferences), &draft.ResolvedReferences); err != nil {
		return nil, err
	}
	if row.Proposal.Valid {
		var p Proposal
		if err := json.Unmarshal([]byte(row.Proposal.String), &p); err == nil {
			draft.Proposal = &p
		}
	}
	if row.Operation.Valid {
		var op Operation
		if err := json.Unmarshal([]byte(row.Operation.String), &op); err == nil {
			draft.Operation = &op
		}
	}
	if row.ChangeContext.Valid {
		if err := json.Unmarshal([]byte(row.ChangeContext.String), &draft.ChangeContext); err != nil {
			return nil, err
		}
	}
	draft.ParentDraftID = value(row.ParentDraftID)
	return draft, nil
}

// editableState computes the draft's editable state: valid only when the
// content is clean under the validator, questions are answered, and the
// manifest's assets are present exactly. Otherwise needs_input.
func editableState(manifest json.RawMessage, questions []Question,
	candidates []Candidate, selected []AssetSelection, findings []Diagnostic, references []ResolvedReference) string {
	for _, d := range findings {
		if d.Severity == "error" {
			return "needs_input"
		}
	}
	for _, q := range questions {
		if q.Answer == "" {
			return "needs_input"
		}
	}
	parsed, err := recipe.Parse(manifest)
	if err != nil {
		return "needs_input"
	}
	for _, asset := range parsed.Assets {
		if !assetSelected(candidates, selected, asset) {
			return "needs_input"
		}
	}
	if !referenceEvidenceReady(parsed, references) {
		return "needs_input"
	}
	return "valid"
}

func assetSelected(candidates []Candidate, selected []AssetSelection, path string) bool {
	for _, c := range candidates {
		if c.Path != path {
			continue
		}
		for _, sel := range selected {
			if sel.Path == path && sel.SHA256 == c.SHA256 {
				return true
			}
		}
	}
	return false
}

// selectManifestAssets derives the exact asset selection for a manifest:
// every declared asset matched to its candidate. Missing assets produce
// findings.
func selectManifestAssets(manifest json.RawMessage, candidates []Candidate) ([]AssetSelection, []Diagnostic) {
	var selected []AssetSelection
	var findings []Diagnostic
	parsed, err := recipe.Parse(manifest)
	if err != nil {
		return selected, nil
	}
	for _, asset := range parsed.Assets {
		found := false
		for _, c := range candidates {
			if c.Path == asset {
				selected = append(selected, AssetSelection{Path: c.Path, SHA256: c.SHA256})
				found = true
				break
			}
		}
		if !found {
			d := Diagnostic{
				Code: "recipe.asset_missing", Severity: "error", Path: asset,
				Message: "manifest asset has no matching candidate", Phase: PhaseInspect, Blocking: true,
			}
			d.ID = DiagnosticID(d, "")
			findings = append(findings, d)
		}
	}
	return selected, findings
}

func allClean(findings []Diagnostic) bool {
	for _, d := range findings {
		if d.Severity == "error" {
			return false
		}
	}
	return true
}

func anyAssetMissing(findings []Diagnostic) bool {
	for _, d := range findings {
		if d.Code == "recipe.asset_missing" {
			return true
		}
	}
	return false
}

// validatorFindings maps the recipe validator's findings to draft diagnostics
// with stable validation-phase IDs.
func (s *Service) validatorFindings(manifest json.RawMessage) []Diagnostic {
	findings, err := s.validator.Validate(manifest)
	if err != nil {
		d := Diagnostic{
			Code: "recipe.draft_manifest_invalid", Severity: "error",
			Message: "manifest is not valid JSON or schema: " + err.Error(),
			Phase:   PhaseValidate, Blocking: true,
		}
		d.ID = DiagnosticID(d, "")
		return []Diagnostic{d}
	}
	out := make([]Diagnostic, 0, len(findings))
	for _, f := range findings {
		d := Diagnostic{
			Code: f.Code, Severity: f.Severity, Path: f.Path, Message: f.Message,
			Phase: PhaseValidate, Blocking: f.Severity == "error",
		}
		d.ID = DiagnosticID(d, "")
		out = append(out, d)
	}
	return out
}

// warningHash hashes the exact current warning tuple so a changed warning can
// never inherit prior consent.
func warningHash(d Diagnostic) string {
	sum := sha256.Sum256([]byte(d.Code + "\x00" + d.Phase + "\x00" + d.Path + "\x00" + d.Message))
	return hex.EncodeToString(sum[:])
}
func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]bool, len(left))
	for _, value := range left {
		values[value] = true
	}
	for _, value := range right {
		if !values[value] {
			return false
		}
	}
	return true
}

// Update applies an operator edit under CAS. It rejects unknown question
// answers, validates warning acknowledgements against the current warning
// set, re-computes validation findings, and preserves inspection findings.
func (s *Service) Update(ctx context.Context, draftID string, version int64, input UpdateRequest) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Version != version {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	if row.State == "installed" {
		return nil, newError("recipe.draft_immutable", "installed drafts are immutable; fork them instead", false)
	}
	if len(input.Manifest) == 0 {
		return nil, newError("recipe.draft_manifest_invalid", "manifest is required", false)
	}
	var manifestObject map[string]json.RawMessage
	if err := json.Unmarshal(input.Manifest, &manifestObject); err != nil || manifestObject == nil {
		return nil, newError("recipe.draft_manifest_invalid", "manifest must be a JSON object", false)
	}
	input.Manifest, err = pinnedManifest(row, input.Manifest)
	if err != nil {
		return nil, err
	}
	draft, err := render(row)
	if err != nil {
		return nil, err
	}
	newSelected, err := validateSelection(input.SelectedAssets, draft.Candidates)
	if err != nil {
		return nil, err
	}
	for _, a := range input.Answers {
		found := false
		for i := range draft.Questions {
			if draft.Questions[i].ID == a.QuestionID {
				draft.Questions[i].Answer = a.Answer
				found = true
				break
			}
		}
		if !found {
			return nil, newError("recipe.question_unknown", "answer references an unknown question", false)
		}
	}
	current := map[string]bool{}
	for _, w := range draft.Diagnostics {
		if w.Severity == "warning" {
			current[warningHash(w)] = true
		}
	}
	kept := draft.AcknowledgedWarnings
	if input.AcknowledgedWarnings != nil {
		kept = []string{}
		seen := map[string]bool{}
		for _, h := range *input.AcknowledgedWarnings {
			if !current[h] {
				return nil, newError("recipe.warning_ack_invalid", "acknowledgement does not match any current warning", false)
			}
			if !seen[h] {
				kept = append(kept, h)
				seen[h] = true
			}
		}
	}
	keptDiags := retainedFindings(row.Diagnostics)
	newFindings := s.validatorFindings(input.Manifest)
	assetFindings := manifestAssetFindings(input.Manifest, draft.Candidates, newSelected)
	newFindings = append(newFindings, assetFindings...)
	diags := append(keptDiags, newFindings...)
	state := editableState(input.Manifest, draft.Questions, draft.Candidates, newSelected, diags, draft.ResolvedReferences)
	SortDiagnostics(diags)
	activeWarnings := make(map[string]bool)
	for _, d := range diags {
		if d.Severity == "warning" {
			activeWarnings[warningHash(d)] = true
		}
	}
	filteredAcks := kept[:0]
	for _, hash := range kept {
		if activeWarnings[hash] {
			filteredAcks = append(filteredAcks, hash)
		}
	}
	kept = filteredAcks

	questionsJSON, err := marshalJSON(draft.Questions)
	if err != nil {
		return nil, err
	}
	selectedJSON, err := marshalJSON(newSelected)
	if err != nil {
		return nil, err
	}
	ackJSON, err := marshalJSON(kept)
	if err != nil {
		return nil, err
	}
	diagJSON, err := marshalJSON(diags)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: state, Source: row.Source,
		ResolvedCommit: row.ResolvedCommit, ResolvedTree: row.ResolvedTree,
		Manifest: string(input.Manifest), Candidates: row.Candidates,
		SelectedAssets: selectedJSON, Diagnostics: diagJSON,
		PackageDigest: row.PackageDigest, RunID: row.RunID, Proposal: row.Proposal,
		ContextSelection: row.ContextSelection, Questions: questionsJSON,
		AcknowledgedWarnings: ackJSON, ResolvedReferences: row.ResolvedReferences,
		ParentDraftID: row.ParentDraftID, ChangeContext: row.ChangeContext,
		ID: row.ID, Version: version,
	})
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	return s.Get(ctx, draftID)
}

// manifestAssetFindings reports manifest assets with no matching selected
// candidate as validation-phase findings.
func manifestAssetFindings(manifest json.RawMessage, candidates []Candidate, selected []AssetSelection) []Diagnostic {
	parsed, err := recipe.Parse(manifest)
	if err != nil {
		return nil
	}
	var findings []Diagnostic
	for _, asset := range parsed.Assets {
		if !assetSelected(candidates, selected, asset) {
			d := Diagnostic{
				Code: "recipe.asset_missing", Severity: "error", Path: asset,
				Message: "manifest asset has no matching selected candidate", Phase: PhaseValidate, Blocking: true,
			}
			d.ID = DiagnosticID(d, "")
			findings = append(findings, d)
		}
	}
	return findings
}

// SetContext persists the operator's assistant context selection.
func (s *Service) SetContext(ctx context.Context, draftID string, version int64, files []ContextFile) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Version != version {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	if row.State == "installed" {
		return nil, newError("recipe.draft_immutable", "installed drafts are immutable; fork them instead", false)
	}
	draft, err := render(row)
	if err != nil {
		return nil, err
	}
	dataByPath := map[string][]byte{}
	normalized := make([]ContextFile, 0, len(files))
	for _, f := range files {
		if f.SourceCommit == "" {
			f.SourceCommit = draft.ResolvedCommit
		}
		inventory, err := contextInventory(draft, f.SourceCommit)
		if err != nil {
			return nil, err
		}
		c, err := findCandidate(inventory, f.Path, f.SHA256, f.Origin)
		if err != nil {
			return nil, newError("recipe.draft_context_mismatch", err.Error(), false)
		}
		f.Origin = c.Origin
		if (f.StartLine == 0) != (f.EndLine == 0) || f.StartLine < 0 || f.EndLine < f.StartLine {
			return nil, newError("recipe.draft_context_invalid", "context range must be empty or 1-based start<=end", false)
		}
		key := contextIdentity(f)
		data, ok := dataByPath[key]
		if !ok {
			data, err = os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+f.SHA256))
			if err != nil {
				return nil, newError("recipe.draft_context_invalid", "cannot read context file: "+err.Error(), false)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != f.SHA256 {
				return nil, newError("recipe.draft_source_changed", "context file hash changed", false)
			}
			if !utf8.Valid(data) {
				return nil, newError("recipe.draft_context_invalid", "context files must contain valid UTF-8 text", false)
			}
			dataByPath[key] = data
		}
		if f.StartLine > 0 && f.EndLine > contextLineCount(data) {
			return nil, newError("recipe.draft_context_invalid", "context range exceeds the file length", false)
		}
		normalized = append(normalized, f)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if contextIdentity(normalized[i]) != contextIdentity(normalized[j]) {
			return contextIdentity(normalized[i]) < contextIdentity(normalized[j])
		}
		if normalized[i].StartLine != normalized[j].StartLine {
			return normalized[i].StartLine < normalized[j].StartLine
		}
		return normalized[i].EndLine < normalized[j].EndLine
	})
	merged := make([]ContextFile, 0, len(normalized))
	for _, f := range normalized {
		if len(merged) == 0 || contextIdentity(merged[len(merged)-1]) != contextIdentity(f) {
			merged = append(merged, f)
			continue
		}
		last := &merged[len(merged)-1]
		if last.StartLine == 0 {
			continue
		}
		if f.StartLine == 0 {
			*last = f
			continue
		}
		if f.StartLine <= last.EndLine+1 {
			if f.EndLine > last.EndLine {
				last.EndLine = f.EndLine
			}
			continue
		}
		merged = append(merged, f)
	}
	perFile := map[string]int{}
	total := 0
	for _, f := range merged {
		key := contextIdentity(f)
		n := contextByteCount(dataByPath[key], f.StartLine, f.EndLine)
		perFile[key] += n
		total += n
		if perFile[key] > maxContextFile {
			return nil, newError("recipe.draft_context_too_large", "selected context exceeds the 64 KiB per-file limit", false)
		}
		if total > maxContextTotal {
			return nil, newError("recipe.draft_context_too_large", "selected context exceeds the 256 KiB total limit", false)
		}
	}
	files = merged
	selectionJSON, err := marshalJSON(files)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: row.State, Source: row.Source,
		ResolvedCommit: row.ResolvedCommit, ResolvedTree: row.ResolvedTree,
		Manifest: row.Manifest, Candidates: row.Candidates,
		SelectedAssets: row.SelectedAssets, Diagnostics: row.Diagnostics,
		PackageDigest: row.PackageDigest, RunID: row.RunID, Proposal: row.Proposal,
		ContextSelection: selectionJSON, Questions: row.Questions,
		AcknowledgedWarnings: row.AcknowledgedWarnings, ResolvedReferences: row.ResolvedReferences,
		ParentDraftID: row.ParentDraftID, ChangeContext: row.ChangeContext,
		ID: row.ID, Version: version,
	})
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	return s.Get(ctx, draftID)
}

func contextLineCount(data []byte) int {
	lines := 1
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		lines--
	}
	return lines
}

func contextByteCount(data []byte, startLine, endLine int) int {
	if startLine == 0 {
		return len(data)
	}
	line := 1
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		if line == endLine {
			return i + 1 - start
		}
		line++
		if line == startLine {
			start = i + 1
		}
	}
	return len(data) - start
}

// lineCount reads the stored candidate blob and returns its line count.
func (s *Service) lineCount(draftID, sha string) (int, error) {
	path := filepath.Join(s.root, draftID, "source", "sha256-"+sha)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return contextLineCount(data), nil
}

// Package executes a reserved package operation. It re-checks strict
// validation, pin, questions, permissions, and assets, then writes the
// deterministic OCI layout to <draftRoot>/packages/<digest>.
func (s *Service) Package(ctx context.Context, draftID, operationID, runID string,
	progress func(phase, message string)) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, PhasePackage)
	if err != nil {
		return nil, err
	}
	if row.State != "analyzing" {
		return nil, newError("recipe.draft_operation_conflict", "draft is not reserved for packaging", false)
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	fail := func(cause error) (*Draft, error) {
		return s.failOperation(ctx, row, op, nil, nil, nil, cause)
	}
	report := func(phase, message string) {
		if progress != nil {
			progress(phase, message)
		}
	}
	draft, err := render(row)
	if err != nil {
		return fail(err)
	}
	report(PhasePackage, "verifying package readiness")
	if err := s.checkReadiness(ctx, draft); err != nil {
		return fail(err)
	}
	assets, err := s.verifySelectedAssets(draft)
	if err != nil {
		return fail(err)
	}
	packed, err := recipe.PackManifest(draft.Manifest, assets, packageSourceAnnotations(draft))
	if err != nil {
		return fail(newError("recipe.draft_package_failed", "manifest packing failed: "+err.Error(), true))
	}
	report(PhasePackage, "packing manifest and assets")
	staging := filepath.Join(s.root, draftID, "staging", op.ID)
	_ = recipe.RemovePackage(staging)
	if err := recipe.WriteLayout(staging, packed); err != nil {
		_ = recipe.RemovePackage(staging)
		return fail(newError("recipe.draft_package_failed", "layout write failed: "+err.Error(), true))
	}
	digestHex := strings.TrimPrefix(packed.ManifestDigest, "sha256:")
	packagesDir := filepath.Join(s.root, draftID, "packages")
	if err := os.MkdirAll(packagesDir, 0o700); err != nil {
		_ = recipe.RemovePackage(staging)
		return fail(newError("recipe.draft_io", err.Error(), true))
	}
	final := filepath.Join(packagesDir, digestHex)
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, final); err != nil {
			_ = recipe.RemovePackage(staging)
			return fail(newError("recipe.draft_io", "cannot move packaged layout: "+err.Error(), true))
		}
	} else {
		_ = recipe.RemovePackage(staging)
	}
	if err := recipe.VerifyLayout(final); err != nil {
		return fail(newError("recipe.draft_package_failed", "packaged layout verification failed: "+err.Error(), true))
	}
	report(PhasePackage, "package ready")
	return s.completeOperation(ctx, row, op, "packaged", json.RawMessage{}, nil, nil, nil,
		packed.ManifestDigest, runID)
}

// checkReadiness enforces the add-to-library gate: strict-clean validation,
// answered questions, and acknowledged warnings.
func (s *Service) checkReadiness(ctx context.Context, draft *Draft) error {
	if err := s.checkChangeBaseline(ctx, draft); err != nil {
		return err
	}
	var source GitSource
	if err := json.Unmarshal(draft.Source, &source); err != nil {
		return err
	}
	if source.Remote != "" && draft.ResolvedCommit != "" {
		pinned, err := pinnedManifest(db.RecipeDraft{Source: string(draft.Source), ResolvedCommit: nullable(draft.ResolvedCommit)}, draft.Manifest)
		if err != nil {
			return err
		}
		if !jsonEqual(pinned, draft.Manifest) {
			return newError("recipe.draft_source_changed", "configuration source must match the server-pinned revision; save the configuration again", false)
		}
	}
	_, findings, err := s.validator.ValidateStrict(draft.Manifest)
	if err != nil {
		return newError("recipe.draft_manifest_invalid", "manifest is not strictly valid: "+err.Error(), false)
	}
	if len(findings) != 0 {
		return newError("recipe.draft_not_strictly_valid", "manifest has validation findings that must be fixed before packaging", false)
	}
	for _, q := range draft.Questions {
		if q.Answer == "" {
			return newError("recipe.draft_questions_unanswered", "all operator questions must be answered before packaging", false)
		}
	}
	ack := map[string]bool{}
	for _, h := range draft.AcknowledgedWarnings {
		ack[h] = true
	}
	for _, d := range draft.Diagnostics {
		if d.Blocking && d.Severity == "error" {
			return newError("recipe.draft_blocking_findings", "blocking findings must be resolved before packaging: "+d.Message, false)
		}
		if d.Severity == "warning" && !ack[warningHash(d)] {
			return newError("recipe.draft_warnings_unacknowledged", "a detected warning is not acknowledged: "+d.Message, false)
		}
	}
	if parsed, parseErr := recipe.Parse(draft.Manifest); parseErr != nil || !referenceEvidenceReady(parsed, draft.ResolvedReferences) {
		return newError("recipe.draft_references_unresolved", "all current image and artifact references require matching verified evidence", false)
	}
	return nil
}

// verifySelectedAssets re-verifies every selected candidate blob and returns
// the exact path-to-bytes asset map.
func (s *Service) verifySelectedAssets(draft *Draft) (map[string][]byte, error) {
	selected := map[string]AssetSelection{}
	for _, sel := range draft.SelectedAssets {
		if _, dup := selected[sel.Path]; dup {
			return nil, newError("recipe.draft_asset_duplicate", "the selection lists an asset more than once: "+sel.Path, false)
		}
		selected[sel.Path] = sel
	}
	assets := map[string][]byte{}
	for _, c := range draft.Candidates {
		sel, ok := selected[c.Path]
		if !ok || sel.SHA256 != c.SHA256 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, draft.ID, "source", "sha256-"+c.SHA256))
		if err != nil {
			return nil, newError("recipe.draft_source_changed", "selected asset is unreadable: "+c.Path, false)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != c.SHA256 {
			return nil, newError("recipe.draft_source_changed", "selected asset hash mismatch: "+c.Path, false)
		}
		assets[c.Path] = data
	}
	parsed, err := recipe.Parse(draft.Manifest)
	if err != nil {
		return nil, newError("recipe.draft_manifest_invalid", "manifest is not parseable: "+err.Error(), false)
	}
	for _, asset := range parsed.Assets {
		if _, ok := assets[asset]; !ok {
			return nil, newError("recipe.draft_source_changed", "selected asset is unavailable: "+asset, false)
		}
	}
	if len(assets) != len(parsed.Assets) {
		return nil, newError("recipe.draft_selection_mismatch", "selected assets must exactly match manifest assets", false)
	}
	return assets, nil
}

// Install executes a reserved install operation. It re-checks readiness,
// imports the packaged layout into the recipe store, and finalizes to
// installed even when the request context is cancelled after the import
// commits. A repeated install of an already-installed identical draft
// returns the same recipe.
func (s *Service) Install(ctx context.Context, draftID, operationID, runID, expectedDigest string,
	expectedAcknowledgements []string, progress func(phase, message string)) (*recipe.Recipe, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, PhaseInstall)
	if err != nil {
		return nil, err
	}
	if row.State != "analyzing" {
		return nil, newError("recipe.draft_operation_conflict", "draft is not reserved for installation", false)
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		_, failure := s.failOperation(ctx, row, op, nil, nil, nil, err)
		return nil, failure
	}
	fail := func(cause error) (*recipe.Recipe, error) {
		if _, fErr := s.failOperation(ctx, row, op, nil, nil, nil, cause); fErr != nil {
			return nil, fErr
		}
		return nil, cause
	}
	report := func(phase, message string) {
		if progress != nil {
			progress(phase, message)
		}
	}
	if s.recipes == nil {
		return fail(newError("recipe.draft_not_packaged", "recipe store is not available", false))
	}
	digest := value(row.PackageDigest)
	if digest == "" {
		return fail(newError("recipe.draft_not_packaged", "draft is not packaged", false))
	}
	if expectedDigest == "" || digest != expectedDigest {
		return fail(newError("recipe.install_digest_mismatch", "package digest does not match the reviewed draft package", false))
	}
	draftAcknowledgements := []string{}
	if err := json.Unmarshal([]byte(row.AcknowledgedWarnings), &draftAcknowledgements); err != nil {
		return fail(err)
	}
	if !sameStringSet(draftAcknowledgements, expectedAcknowledgements) {
		return fail(newError("recipe.install_warning_consent_mismatch", "warning consent does not match the reviewed draft version", false))
	}
	if op.PreviousState == "installed" {
		current, err := s.isCurrentPackage(ctx, digest)
		if err != nil {
			return fail(err)
		}
		if !current {
			return fail(newError("recipe.draft_stale_version", "a newer saved recipe is selected; the prior save cannot be replayed", false))
		}
		if detail, lookupErr := s.recipes.Get(ctx, digest); lookupErr == nil {
			if _, doneErr := s.completeOperation(ctx, row, op, "installed", json.RawMessage{}, nil, nil, nil, detail.Digest, runID); doneErr != nil {
				return nil, doneErr
			}
			return &detail.Recipe, nil
		}
	}
	draft, err := render(row)
	if err != nil {
		return fail(err)
	}
	report(PhaseInstall, "verifying install readiness")
	if err := s.checkReadiness(ctx, draft); err != nil {
		return fail(err)
	}
	if _, err := s.verifySelectedAssets(draft); err != nil {
		return fail(err)
	}
	packageDir := filepath.Join(s.root, draftID, "packages", strings.TrimPrefix(digest, "sha256:"))
	if _, err := os.Stat(packageDir); err != nil {
		return fail(newError("recipe.draft_source_changed", "the packaged layout is missing or unreadable", false))
	}
	report(PhaseInstall, "importing packaged recipe")
	layout, err := recipe.ReadLayout(packageDir)
	if err != nil || layout.ManifestDigest != digest || !jsonEqual(layout.ConfigJSON, draft.Manifest) {
		return fail(newError("recipe.draft_source_changed", "packaged content no longer matches the reviewed draft", false))
	}
	var source GitSource
	if err := json.Unmarshal(draft.Source, &source); err != nil {
		return fail(err)
	}
	repository, expected := "", ""
	if draft.ChangeContext != nil {
		repository, expected = draft.ChangeContext.RepositoryID, draft.ChangeContext.ExpectedCurrentDigest
	}
	if repository == "" {
		expected = ""
	}
	installed, err := s.recipes.ImportReviewed(ctx, recipe.RecipeSource{Type: "local", Path: packageDir,
		Remote: source.Remote, Revision: draft.ResolvedCommit, Tree: draft.ResolvedTree,
		SourcePath: source.Path, TrackingRef: source.Revision}, repository, expected)
	if err != nil {
		return fail(err)
	}
	terminalCtx, cancelTerminal := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelTerminal()
	if link, linkErr := s.q.GetRecipeRepositoryVersionByDigest(terminalCtx, installed.Digest); linkErr == nil {
		change := draft.ChangeContext
		if change == nil {
			change = &ChangeContext{Kind: "add", BaseSourceStatus: "unavailable"}
		}
		change.RepositoryID = link.RepositoryID
		encoded, encodeErr := marshalJSON(change)
		if encodeErr != nil {
			return nil, encodeErr
		}
		row.ChangeContext = nullable(encoded)
	}
	report(PhaseInstall, "recipe installed")
	if _, err := s.completeOperation(terminalCtx, row, op, "installed", json.RawMessage{}, nil, nil, nil, installed.Digest, runID); err != nil {
		return nil, err
	}
	return &installed, nil
}

// Delete removes a draft and its service-owned state. It requires a matching
// version and refuses to delete a draft that owns an active operation.
func (s *Service) Delete(ctx context.Context, draftID string, version int64) error {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return err
	}
	if row.Operation.Valid {
		return newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	rows, err := s.q.DeleteRecipeDraft(ctx, db.DeleteRecipeDraftParams{ID: draftID, Version: version})
	if err != nil {
		return err
	}
	if rows != 1 {
		return newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
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

// ReconcileOperations restores interrupted operations after a restart. An
// interrupted inspect becomes failed; every other kind returns to the state
// it was reserved from, except an install whose exact package digest is
// already in the recipe store, which finalizes as installed. Each is
// annotated with recipe.operation_interrupted and never auto-replayed.
func (s *Service) ReconcileOperations(ctx context.Context) (int, error) {
	rows, err := s.q.ListRecipeDrafts(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if !row.Operation.Valid {
			if row.State == "analyzing" {
				diags := retainedFindings(row.Diagnostics)
				d := Diagnostic{Code: "recipe.operation_interrupted", Severity: "error", Phase: PhaseGenerate,
					Message: "the operation was interrupted and was not re-run", Blocking: true, Retryable: true, Dismissible: true}
				d.ID = DiagnosticID(d, row.ID)
				diags = append(diags, d)
				encoded, _ := marshalJSON(diags)
				changed, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET state='needs_input', diagnostics=?,
version=version+1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE id=? AND version=? AND state='analyzing' AND operation IS NULL`, encoded, row.ID, row.Version)
				if err != nil {
					return n, err
				}
				count, err := changed.RowsAffected()
				if err != nil {
					return n, err
				}
				n += int(count)
			}
			continue
		}
		var op Operation
		if err := json.Unmarshal([]byte(row.Operation.String), &op); err != nil || op.ID == "" {
			continue
		}
		state := op.PreviousState
		if op.Kind == PhaseInspect {
			state = "failed"
		}
		if op.Kind == PhaseInstall && value(row.PackageDigest) != "" && s.recipes != nil {
			current, currentErr := s.isCurrentPackage(ctx, value(row.PackageDigest))
			if _, lookupErr := s.recipes.Get(ctx, value(row.PackageDigest)); lookupErr == nil && currentErr == nil && current {
				state = "installed"
			}
		}
		var diags []Diagnostic
		if err := json.Unmarshal([]byte(row.Diagnostics), &diags); err != nil {
			diags = []Diagnostic{}
		}
		d := Diagnostic{
			Code: "recipe.operation_interrupted", Severity: "error",
			Message: "the operation was interrupted by a restart and was not re-run",
			Phase:   op.Kind, Blocking: true, Retryable: true, Dismissible: true,
		}
		d.ID = DiagnosticID(d, op.ID)
		diags = append(diags, d)
		if _, err := s.completeOperation(ctx, row, &op, state, json.RawMessage{}, nil, nil, diags,
			value(row.PackageDigest), value(row.RunID)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// completeOperation applies the terminal state update for an active
// operation, bumping the version once and clearing the operation record.
// Nil candidates, selected, or diagnostics fall back to the row's persisted
// values; an empty manifest leaves the stored manifest unchanged.
func (s *Service) completeOperation(ctx context.Context, row db.RecipeDraft, op *Operation,
	state string, manifest json.RawMessage, candidates []Candidate,
	selected []AssetSelection, diags []Diagnostic, packageDigest, runID string) (*Draft, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if candidates == nil {
		if err := json.Unmarshal([]byte(row.Candidates), &candidates); err != nil {
			candidates = []Candidate{}
		}
	}
	if selected == nil {
		if err := json.Unmarshal([]byte(row.SelectedAssets), &selected); err != nil {
			selected = []AssetSelection{}
		}
	}
	if diags == nil {
		if err := json.Unmarshal([]byte(row.Diagnostics), &diags); err != nil {
			diags = []Diagnostic{}
		}
	}
	if len(manifest) == 0 {
		manifest = json.RawMessage(row.Manifest)
	}
	candidatesJSON, err := marshalJSON(candidates)
	if err != nil {
		return nil, err
	}
	selectedJSON, err := marshalJSON(selected)
	if err != nil {
		return nil, err
	}
	SortDiagnostics(diags)
	diagsJSON, err := marshalJSON(diags)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.CompleteRecipeDraftOperation(ctx, db.CompleteRecipeDraftOperationParams{
		State: state, Source: row.Source,
		ResolvedCommit: row.ResolvedCommit, ResolvedTree: row.ResolvedTree,
		Manifest: string(manifest), Candidates: candidatesJSON, SelectedAssets: selectedJSON,
		Diagnostics: diagsJSON, PackageDigest: nullable(packageDigest), RunID: nullable(runID),
		Proposal: row.Proposal, ContextSelection: row.ContextSelection,
		Questions: row.Questions, AcknowledgedWarnings: row.AcknowledgedWarnings,
		ResolvedReferences: row.ResolvedReferences, ParentDraftID: row.ParentDraftID, ChangeContext: row.ChangeContext,
		ID: row.ID, Operation: nullable(op.ID),
	})
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_operation_lost", "the active operation changed before completion", false)
	}
	return s.Get(ctx, row.ID)
}

// CreateFromDir persists a draft from a local directory. It is retained for
// internal and test callers that bypass the git inspect path.
func (s *Service) CreateFromDir(ctx context.Context, source any, resolvedCommit, resolvedTree string,
	manifest json.RawMessage, sourceDir string) (*Draft, error) {
	draftID, err := id.New()
	if err != nil {
		return nil, err
	}
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
	candidates, exclusions, diagnostics, err := s.ingestAndCopy(sourceDir, candidateRoot)
	if err != nil {
		return nil, err
	}
	_ = writeExclusions(filepath.Join(draftRoot, "exclusions.json"), exclusions)
	sourceJSON, err := marshalJSON(source)
	if err != nil {
		return nil, err
	}
	candidateJSON, err := marshalJSON(candidates)
	if err != nil {
		return nil, err
	}
	diagnosticJSON, err := marshalJSON(diagnostics)
	if err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		manifest = json.RawMessage(defaultManifest)
	}
	state := "needs_input"
	if resolvedCommit != "" {
		state = editableState(manifest, []Question{}, candidates, []AssetSelection{}, s.validatorFindings(manifest), nil)
	}
	if err := s.q.CreateRecipeDraft(ctx, db.CreateRecipeDraftParams{
		ID: draftID, State: state, Source: sourceJSON,
		ResolvedCommit: nullable(resolvedCommit), ResolvedTree: nullable(resolvedTree),
		Manifest: string(manifest), Candidates: candidateJSON,
		SelectedAssets: "[]", Diagnostics: diagnosticJSON,
		ContextSelection: "[]", Questions: "[]", AcknowledgedWarnings: "[]",
		ResolvedReferences: "[]",
	}); err != nil {
		return nil, err
	}
	cleanup = false
	return s.Get(ctx, draftID)
}

func nullable(value string) sql.NullString { return sql.NullString{String: value, Valid: value != ""} }

func value(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func repositoryID(source GitSource) string {
	u, err := url.Parse(source.Remote)
	if err != nil {
		return ""
	}
	segments := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, "/"), "/"), "/")
	if len(segments) != 2 {
		return ""
	}
	return segments[0] + "/" + segments[1]
}

func withDirectoryBudget(parent context.Context, root string, limit int64) (context.Context, func() bool, func()) {
	ctx, cancel := context.WithCancel(parent)
	var exceeded atomic.Bool
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				size, err := treeSize(ctx, root)
				if err == nil && size > limit {
					exceeded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	stop := func() {
		select {
		case <-done:
		default:
			close(done)
		}
		cancel()
	}
	return ctx, exceeded.Load, stop
}

func treeSize(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// limitedWriter bounds subprocess diagnostics retained in memory.
type limitedWriter struct {
	data []byte
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := gitOutputLimit - len(w.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.data = append(w.data, p...)
	}
	return n, nil
}

func isolatedGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_") || upper == "SSH_ASKPASS" ||
			upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" ||
			upper == "NO_PROXY" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
}

func isolatedGitArgs(args []string) []string {
	config := []string{
		"-c", "credential.helper=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "http.followRedirects=false",
	}
	return append(config, args...)
}

// runGitEnv runs git with ambient config, proxy overrides, credential helpers,
// hooks, redirects, and non-HTTPS helper protocols disabled.
func runGitEnv(ctx context.Context, dir string, args ...string) error {
	command := exec.CommandContext(ctx, "git", isolatedGitArgs(args)...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = isolatedGitEnv()
	var output limitedWriter
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output.data)))
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", isolatedGitArgs(args)...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = isolatedGitEnv()
	var stdout, stderr limitedWriter
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(stderr.data)))
	}
	return strings.TrimSpace(string(stdout.data)), nil
}

// validateSelection checks an operator asset selection against the retained
// inventory: every selection must name an inventoried path with the
// matching content hash, with no duplicates.
func validateSelection(input []AssetSelection, candidates []Candidate) ([]AssetSelection, error) {
	seen := map[string]bool{}
	out := []AssetSelection{}
	for _, sel := range input {
		if sel.Path == "" || sel.SHA256 == "" {
			return nil, newError("recipe.draft_invalid_selection", "each selected asset needs a path and sha256", false)
		}
		c, err := findCandidate(candidates, sel.Path, sel.SHA256, sel.Origin)
		if err != nil {
			return nil, err
		}
		sel.Origin = c.Origin
		if seen[sel.Path] {
			return nil, newError("recipe.draft_asset_duplicate", "the selection lists an asset more than once: "+sel.Path, false)
		}
		seen[sel.Path] = true
		out = append(out, sel)
	}
	return out, nil
}
