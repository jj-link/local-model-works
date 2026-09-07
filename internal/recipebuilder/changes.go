package recipebuilder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// ChangeRequest selects a saved baseline, never a moving running version.
type ChangeRequest struct {
	Kind                  string `json:"kind"`
	ExpectedCurrentDigest string `json:"expected_current_digest"`
	ExpectedHeadCommit    string `json:"expected_head_commit,omitempty"`
	DeploymentID          string `json:"deployment_id,omitempty"`
}

// AllocateChange retains the verified package before creating editable work.
// Repair never contacts the source repository. Update resolves its immutable
// target before any checkout and preserves the saved manifest as the baseline.
func (s *Service) AllocateChange(ctx context.Context, baseDigest string, input ChangeRequest) (*Draft, error) {
	if input.Kind != "repair" && input.Kind != "update" {
		return nil, newError("recipe.draft_change_invalid", "kind must be repair or update", false)
	}
	if s.recipes == nil {
		return nil, recipe.ErrUnknown
	}
	previous, err := s.recipes.Get(ctx, baseDigest)
	if err != nil {
		return nil, err
	}
	change := ChangeContext{Kind: input.Kind, BaseRecipeDigest: baseDigest, ExpectedCurrentDigest: input.ExpectedCurrentDigest, BaseManifest: previous.Manifest, BaseSourceStatus: "unavailable", DeploymentID: input.DeploymentID}
	var source GitSource
	var tree string
	link, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, baseDigest)
	if err == nil {
		repository, err := s.q.GetRecipeRepository(ctx, link.RepositoryID)
		if err != nil {
			return nil, err
		}
		if input.ExpectedCurrentDigest == "" || input.ExpectedCurrentDigest != value(repository.CurrentDigest) {
			return nil, newError("recipe.draft_stale_version", "saved recipe changed; reload before preparing changes", false)
		}
		change.RepositoryID, change.BaseCommit, change.BaseTree = link.RepositoryID, link.CommitSha, value(link.TreeSha)
		source = GitSource{Remote: repository.SourceUrl, Path: repository.SourcePath, Revision: repository.TrackingRef}
		if source.Path == "." {
			source.Path = ""
		}
		if source.Revision == "HEAD" {
			source.Revision = ""
		}
		change.BaseSource = &GitSource{Remote: source.Remote, Path: source.Path, Revision: link.CommitSha}
		tree = change.BaseTree
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	} else if input.ExpectedCurrentDigest != baseDigest {
		return nil, newError("recipe.draft_stale_version", "expected current digest must identify this saved package", false)
	}
	if input.DeploymentID != "" {
		deployment, err := s.q.GetDeployment(ctx, input.DeploymentID)
		if err != nil || deployment.RecipeDigest != baseDigest {
			return nil, newError("recipe.draft_deployment_mismatch", "deployment does not use the selected saved recipe", false)
		}
	}
	target := change.BaseCommit
	if input.Kind == "update" {
		if change.RepositoryID == "" {
			return nil, newError("recipe.source_unavailable", "this saved package has no verified repository source", false)
		}
		// Reuse the same URL/ref validation as Add, without allocating an extra draft.
		if !safeChangeSource(source) {
			return nil, newError("recipe.source_invalid", "updates require a normalized HTTPS GitHub source", false)
		}
		target = strings.ToLower(input.ExpectedHeadCommit)
		if target == "" {
			resolveCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
			target, err = resolveRevision(resolveCtx, source.Remote, source.Revision)
			cancel()
			if err != nil {
				return nil, err
			}
		}
		if !commitRE.MatchString(target) {
			return nil, newError("recipe.source_invalid", "target must be a full immutable commit", false)
		}
		tree = ""
	}
	packed, err := s.recipes.ReadPackage(ctx, baseDigest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, recipe.ErrUnknown
	}
	if err != nil {
		return nil, err
	}
	if !jsonEqual(packed.ConfigJSON, previous.Manifest) {
		return nil, newError("recipe.draft_source_changed", "saved package manifest differs from its library record", false)
	}
	draftID, err := id.New()
	if err != nil {
		return nil, err
	}
	helpers, err := s.importCompiledAssets(draftID, packed, "baseline")
	if err != nil {
		return nil, err
	}
	// All retained blobs are verified and bounded. Missing historical evidence
	// does not prevent preserving the package's own configuration and helpers.
	inventory, parentID, diagnostics, retainErr := s.retainChangeSource(ctx, draftID, baseDigest, change.BaseCommit)
	if retainErr == nil && parentID != "" {
		change.BaseSourceStatus = "available"
	} else {
		change.BaseSourceError = "Original repository files are unavailable; the saved configuration and helper files are still available."
	}
	change.BaseCandidates = inventory
	candidates := append(append([]Candidate{}, inventory...), helpers...)
	selected := make([]AssetSelection, 0, len(helpers))
	for _, helper := range helpers {
		selected = append(selected, AssetSelection{Path: helper.Path, SHA256: helper.SHA256, Origin: helper.Origin})
	}
	change.BaseAssets = selected
	// Old source bytes are evidence, never silently selected as package assets.
	if input.Kind == "update" {
		candidates = helpers
	}
	sourceJSON, _ := marshalJSON(source)
	contextJSON, _ := marshalJSON(change)
	candidateJSON, _ := marshalJSON(candidates)
	selectedJSON, _ := marshalJSON(selected)
	diagnosticJSON, _ := marshalJSON(diagnostics)
	err = s.q.CreateRecipeDraft(ctx, db.CreateRecipeDraftParams{ID: draftID, State: "needs_input", Source: sourceJSON,
		ResolvedCommit: nullable(target), ResolvedTree: nullable(tree), Manifest: string(previous.Manifest), Candidates: candidateJSON,
		SelectedAssets: selectedJSON, Diagnostics: diagnosticJSON, ContextSelection: "[]", Questions: "[]", AcknowledgedWarnings: "[]", ResolvedReferences: "[]",
		ParentDraftID: nullable(parentID), ChangeContext: nullable(contextJSON)})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, draftID)
}

func safeChangeSource(source GitSource) bool {
	identity, remote, _, err := recipe.RepositoryIdentity(recipe.Source{URL: source.Remote, Path: source.Path})
	if err != nil || identity == "" || remote != source.Remote {
		return false
	}
	prefix := "https://github.com/"
	if !strings.HasPrefix(remote, prefix) {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(remote, prefix), "/")
	return len(segments) == 2 && segments[0] != "" && segments[1] != "" && validRevision(source.Revision)
}

func (s *Service) retainChangeSource(ctx context.Context, draftID, digest, commit string) ([]Candidate, string, []Diagnostic, error) {
	rows, err := s.q.ListRecipeDraftsByPackageDigest(ctx, nullable(digest))
	if err != nil {
		return nil, "", nil, err
	}
	for _, row := range rows {
		if value(row.ResolvedCommit) != commit {
			continue
		}
		draft, err := render(row)
		if err != nil {
			continue
		}
		inventory := make([]Candidate, 0)
		var total int64
		valid := true
		for _, candidate := range draft.Candidates {
			if candidate.Origin != OriginSource {
				continue
			}
			total += candidate.Size
			if len(inventory) >= maxCandidates || candidate.Size > maxFileBytes || total > maxTotalBytes {
				valid = false
				break
			}
			original := filepath.Join(s.root, row.ID, "source", "sha256-"+candidate.SHA256)
			info, err := os.Lstat(original)
			if err != nil || !info.Mode().IsRegular() || info.Size() != candidate.Size {
				valid = false
				break
			}
			hash, binary, err := storeCandidate(original, filepath.Join(s.root, draftID, "source"))
			if err != nil || hash != candidate.SHA256 {
				valid = false
				break
			}
			candidate.Binary = binary
			inventory = append(inventory, candidate)
		}
		if valid && len(inventory) > 0 {
			return inventory, row.ID, retainedFindings(row.Diagnostics), nil
		}
	}
	return []Candidate{}, "", []Diagnostic{}, nil
}

// PrepareChange executes the initial recipe-change ledger entry. An explicit
// subsequent Inspect is the only way a repair fetches unavailable old source.
func (s *Service) PrepareChange(ctx context.Context, draftID, operationID, runID string, progress func(string, string)) (*Draft, error) {
	return s.prepareChange(ctx, draftID, operationID, runID, progress, false)
}

func (s *Service) prepareChange(ctx context.Context, draftID, operationID, runID string, progress func(string, string), inspectSource bool) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, PhaseInspect)
	if err != nil {
		return nil, err
	}
	draft, err := render(row)
	if err != nil {
		return nil, err
	}
	if draft.ChangeContext == nil {
		return nil, newError("recipe.draft_change_invalid", "draft has no saved baseline", false)
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	fail := func(cause error) (*Draft, error) { return s.failOperation(ctx, row, op, nil, nil, nil, cause) }
	change := draft.ChangeContext
	if change.Kind == "repair" && !inspectSource {
		return s.completeOperation(ctx, row, op, "needs_input", nil, nil, nil, nil, "", runID)
	}
	var source GitSource
	if err := json.Unmarshal(draft.Source, &source); err != nil {
		return fail(err)
	}
	if !safeChangeSource(source) || !commitRE.MatchString(draft.ResolvedCommit) {
		return fail(newError("recipe.source_unavailable", "no verified pinned GitHub source is available for inspection", false))
	}
	if progress != nil {
		progress(PhaseInspect, "inspecting the frozen source revision")
	}
	_, _, inspectDir, err := s.checkoutPinned(ctx, draftID, op.ID, source, draft.ResolvedCommit)
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(filepath.Join(s.root, draftID, "inspection"))
	row, err = s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	inventory, exclusions, findings, err := s.ingestAndCopy(inspectDir, filepath.Join(s.root, draftID, "source"))
	if err != nil {
		return fail(err)
	}
	if err := writeExclusions(filepath.Join(s.root, draftID, "exclusions.json"), exclusions); err != nil {
		return fail(err)
	}
	// Preserve baseline helpers even where new upstream source reuses their paths.
	candidates := append([]Candidate{}, inventory...)
	for _, c := range draft.Candidates {
		if c.Origin != OriginSource {
			candidates = append(candidates, c)
		}
	}
	findings = append(retainedFindings(row.Diagnostics), findings...)
	if change.Kind == "repair" {
		change.BaseCandidates, change.BaseSourceStatus, change.BaseSourceError = inventory, "available", ""
		encoded, _ := marshalJSON(change)
		row.ChangeContext = nullable(encoded)
	} else if s.registry != nil {
		previous, err := s.recipes.Get(ctx, change.BaseRecipeDigest)
		if err != nil {
			return fail(err)
		}
		repoSource := recipe.RepositorySource{RepositoryID: change.RepositoryID, URL: source.Remote, Path: source.Path, CommitSHA: value(row.ResolvedCommit), TreeSHA: value(row.ResolvedTree)}
		checkout := filepath.Join(s.root, draftID, "inspection", op.ID, "checkout")
		if compiler, ok := s.registry.Lookup(repoSource, checkout); ok {
			packed, compileErr := compiler.Compile(ctx, repoSource, checkout, &previous)
			if compileErr != nil {
				finding := Diagnostic{Code: "recipe.compile_failed", Severity: "warning", Message: compileErr.Error(), Phase: PhaseInspect, Retryable: true}
				finding.ID = DiagnosticID(finding, op.ID)
				findings = append(findings, finding)
			} else {
				proposal, err := s.compilerChangeProposal(row, op, runID, packed)
				if err != nil {
					return fail(err)
				}
				encoded, err := marshalJSON(proposal)
				if err != nil {
					return fail(err)
				}
				row.Proposal = nullable(encoded)
			}
		}
	}
	manifest, err := pinnedManifest(row, draft.Manifest)
	if err != nil {
		return fail(err)
	}
	findings = append(findings, s.validatorFindings(manifest)...)
	return s.completeOperation(ctx, row, op, "needs_input", manifest, candidates, nil, findings, "", runID)
}

func (s *Service) compilerChangeProposal(row db.RecipeDraft, op *Operation, runID string, packed *recipe.PackResult) (*Proposal, error) {
	generated, err := s.importCompiledAssets(row.ID, packed, op.ID)
	if err != nil {
		return nil, err
	}
	proposalID, err := id.New()
	if err != nil {
		return nil, err
	}
	manifest, err := pinnedManifest(row, packed.ConfigJSON)
	if err != nil {
		return nil, err
	}
	proposal := &Proposal{ID: proposalID, BaseVersion: row.Version + 1, ProviderID: "compiler", RunID: runID, Manifest: manifest, Files: []ProposalFile{}, Questions: []Question{}, Evidence: []Evidence{}, SelectedSourceAssets: []AssetSelection{}, Summary: "Suggested configuration from the repository compiler; saved and running recipes remain unchanged.", Diagnostics: s.validatorFindings(manifest)}
	for _, candidate := range generated {
		body, err := os.ReadFile(filepath.Join(s.root, row.ID, "source", "sha256-"+candidate.SHA256))
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(body) || containsNUL(body) {
			return nil, newError("recipe.draft_asset_invalid", "compiler helper is not reviewable UTF-8: "+candidate.Path, false)
		}
		proposal.Files = append(proposal.Files, ProposalFile{Path: candidate.Path, Content: string(body), SourcePath: candidate.SourcePath})
	}
	return proposal, nil
}

// Comparison is an immutable-source diff and an independently editable config diff.
type Comparison struct {
	BaseRecipeDigest     string                `json:"base_recipe_digest,omitempty"`
	BaseCommit           string                `json:"base_commit,omitempty"`
	TargetCommit         string                `json:"target_commit,omitempty"`
	BaseSourceStatus     string                `json:"base_source_status"`
	Files                []SourceFileChange    `json:"files"`
	ConfigurationChanges []ConfigurationChange `json:"configuration_changes"`
}
type SourceFileChange struct {
	Path         string `json:"path"`
	Change       string `json:"change"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	BeforeSize   *int64 `json:"before_size,omitempty"`
	AfterSize    *int64 `json:"after_size,omitempty"`
	Binary       bool   `json:"binary"`
}
type ConfigurationChange struct {
	Path   string          `json:"path"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

func (s *Service) Compare(ctx context.Context, draftID string) (*Comparison, error) {
	draft, err := s.Get(ctx, draftID)
	if err != nil {
		return nil, err
	}
	result := &Comparison{TargetCommit: draft.ResolvedCommit, BaseSourceStatus: "unavailable", Files: []SourceFileChange{}, ConfigurationChanges: []ConfigurationChange{}}
	if draft.ChangeContext == nil {
		return result, nil
	}
	change := draft.ChangeContext
	result.BaseRecipeDigest, result.BaseCommit, result.BaseSourceStatus = change.BaseRecipeDigest, change.BaseCommit, change.BaseSourceStatus
	if change.BaseSourceStatus == "available" && draft.ResolvedTree != "" {
		before, after := map[string]Candidate{}, map[string]Candidate{}
		for _, c := range change.BaseCandidates {
			if c.Origin == OriginSource {
				before[c.Path] = c
			}
		}
		for _, c := range draft.Candidates {
			if c.Origin == OriginSource {
				after[c.Path] = c
			}
		}
		paths := map[string]bool{}
		for p := range before {
			paths[p] = true
		}
		for p := range after {
			paths[p] = true
		}
		for p := range paths {
			b, bok := before[p]
			a, aok := after[p]
			if bok && aok && b.SHA256 == a.SHA256 {
				continue
			}
			item := SourceFileChange{Path: p, Change: "modified", BeforeSHA256: b.SHA256, AfterSHA256: a.SHA256, Binary: b.Binary || a.Binary}
			if bok {
				size := b.Size
				item.BeforeSize = &size
			}
			if aok {
				size := a.Size
				item.AfterSize = &size
			}
			if !bok {
				item.Change = "added"
			}
			if !aok {
				item.Change = "removed"
			}
			result.Files = append(result.Files, item)
		}
		sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	}
	var before any = map[string]any{}
	var after any
	target := draft.Manifest
	if draft.Proposal != nil {
		target = draft.Proposal.Manifest
	}
	// New recipes have no saved configuration; their fields are additions.
	if change.Kind != "add" || len(change.BaseManifest) != 0 {
		if err := json.Unmarshal(change.BaseManifest, &before); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(target, &after); err != nil {
		return nil, err
	}
	compareConfiguration("", before, true, after, true, &result.ConfigurationChanges)
	return result, nil
}
func compareConfiguration(path string, before any, bok bool, after any, aok bool, out *[]ConfigurationChange) {
	if bok && aok && reflect.DeepEqual(before, after) {
		return
	}
	b, bmap := before.(map[string]any)
	a, amap := after.(map[string]any)
	if bok && aok && bmap && amap {
		keys := map[string]bool{}
		for k := range b {
			keys[k] = true
		}
		for k := range a {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			bv, bo := b[k]
			av, ao := a[k]
			compareConfiguration(path+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"), bv, bo, av, ao, out)
		}
		return
	}
	item := ConfigurationChange{Path: path}
	if bok {
		item.Before, _ = json.Marshal(before)
	}
	if aok {
		item.After, _ = json.Marshal(after)
	}
	*out = append(*out, item)
}
func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func (s *Service) checkChangeBaseline(ctx context.Context, draft *Draft) error {
	change := draft.ChangeContext
	if change == nil || change.BaseRecipeDigest == "" {
		return nil
	}
	if s.recipes == nil {
		return recipe.ErrUnknown
	}
	base, err := s.recipes.ReadPackage(ctx, change.BaseRecipeDigest)
	if errors.Is(err, os.ErrNotExist) {
		return recipe.ErrUnknown
	}
	if err != nil {
		return err
	}
	if !jsonEqual(base.ConfigJSON, change.BaseManifest) {
		return newError("recipe.draft_source_changed", "saved baseline no longer matches the verified package", false)
	}
	if change.RepositoryID != "" {
		link, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, change.BaseRecipeDigest)
		if err != nil {
			return err
		}
		if link.RepositoryID != change.RepositoryID {
			return newError("recipe.draft_source_changed", "saved baseline belongs to another repository", false)
		}
		repository, err := s.q.GetRecipeRepository(ctx, change.RepositoryID)
		if err != nil {
			return err
		}
		if value(repository.CurrentDigest) != change.ExpectedCurrentDigest {
			return newError("recipe.draft_stale_version", "saved recipe changed; review a new change before saving", false)
		}
	}
	return nil
}

func packageSourceAnnotations(draft *Draft) map[string]string {
	var source GitSource
	if json.Unmarshal(draft.Source, &source) != nil || source.Remote == "" || draft.ResolvedCommit == "" {
		return nil
	}
	tracking := source.Revision
	if tracking == "" {
		tracking = "HEAD"
	}
	return map[string]string{
		"dev.localmodelworks.source.url":          source.Remote,
		"dev.localmodelworks.source.path":         source.Path,
		"dev.localmodelworks.source.revision":     draft.ResolvedCommit,
		"dev.localmodelworks.source.tree":         draft.ResolvedTree,
		"dev.localmodelworks.source.tracking-ref": tracking,
	}
}

// CheckSaveBaseline provides a synchronous stale-base response before a save
// operation is queued. ImportReviewed repeats the current-pointer CAS in its
// storage transaction, so this check never replaces transactional ownership.
func (s *Service) CheckSaveBaseline(ctx context.Context, draftID string) error {
	draft, err := s.Get(ctx, draftID)
	if err != nil {
		return err
	}
	if draft.State == "installed" {
		current, err := s.isCurrentPackage(ctx, draft.PackageDigest)
		if err != nil {
			return err
		}
		if !current {
			return newError("recipe.draft_stale_version", "a newer saved recipe is selected; the prior save cannot be replayed", false)
		}
		return nil
	}
	if draft.ChangeContext == nil || draft.ChangeContext.BaseRecipeDigest == "" {
		var source GitSource
		if err := json.Unmarshal(draft.Source, &source); err != nil {
			return err
		}
		if source.Remote != "" {
			repositoryID := repositoryID(source)
			if _, err := s.q.GetRecipeRepository(ctx, repositoryID); err == nil {
				return &recipe.PackError{Code: "recipe.repository_exists", Message: "This repository is already in the library; open its recipe instead: " + repositoryID}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	return s.checkChangeBaseline(ctx, draft)
}

func (s *Service) isCurrentPackage(ctx context.Context, digest string) (bool, error) {
	link, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	repository, err := s.q.GetRecipeRepository(ctx, link.RepositoryID)
	if err != nil {
		return false, err
	}
	return value(repository.CurrentDigest) == digest, nil
}
