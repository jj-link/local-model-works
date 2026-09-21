package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipe/repositorycompiler"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

func upstreamManifest(raw json.RawMessage) bool {
	var manifest recipe.Manifest
	if json.Unmarshal(raw, &manifest) != nil {
		return false
	}
	for _, workload := range manifest.Workloads {
		if workload.Upstream != nil {
			return true
		}
	}
	return false
}

// Default context is disclosed in GenerationRequest before provider consent.
// Only retained source text is eligible; saved/generated helpers are not source
// evidence. Explicit selections replace these defaults and retain their ranges.
func changeSourceContext(draft *Draft) ([]ContextFile, string) {
	var selected []ContextFile
	var omitted []string
	total := int64(0)
	for _, candidate := range draft.Candidates {
		if candidate.Origin != OriginSource || candidate.Binary || excludedFile(path.Base(candidate.Path)) != "" {
			continue
		}
		name := strings.ToLower(path.Base(candidate.Path))
		ext := strings.ToLower(path.Ext(name))
		relevant := strings.HasPrefix(name, "readme") || strings.HasPrefix(name, "dockerfile") || name == "makefile" || name == ".env.example" || name == ".env.sample"
		switch ext {
		case ".md", ".rst", ".sh", ".bash", ".py", ".yaml", ".yml", ".toml", ".json", ".example", ".sample":
			relevant = true
		}
		if !relevant {
			continue
		}
		if candidate.Size > maxContextFile || total+candidate.Size > maxContextTotal {
			omitted = append(omitted, candidate.Path)
			continue
		}
		selected = append(selected, ContextFile{Path: candidate.Path, SHA256: candidate.SHA256, Origin: OriginSource, SourceCommit: draft.ResolvedCommit})
		total += candidate.Size
	}
	status := "Pinned source instructions, entrypoints and configuration are included in the reviewed context; other repository files have not been sent."
	if len(selected) == 0 {
		status = "Pinned source instructions are unavailable. Inspect the source or select retained source evidence; ask explicit questions instead of reconstructing the saved launch helper."
	}
	if len(omitted) != 0 {
		status += " Source files outside context budgets (select explicit line ranges to include them): " + strings.Join(omitted, ", ")
	}
	return selected, status
}

// Runnable source-backed suggestions must either preserve an authored native
// manifest or describe source-owned execution. A helper is never evidence that
// its reconstructed stack is the repository's launch contract.
func validateSourceOwnedProposal(result recipeassistant.Result, request recipeassistant.Request) error {
	var manifest recipe.Manifest
	if err := json.Unmarshal(result.Manifest, &manifest); err != nil {
		return err
	}
	pins, _ := json.Marshal(request.SourcePins)
	var sourcePins struct {
		Commit string `json:"commit"`
	}
	_ = json.Unmarshal(pins, &sourcePins)
	if upstreamManifest(request.Manifest) && !upstreamManifest(result.Manifest) {
		return newError("recipe.proposal_upstream_required", "A source-owned recipe cannot be replaced with an application-generated serving stack.", false)
	}
	// Exact authored native manifests need no synthetic field citations, even
	// when their authored runtime itself uses upstream execution.
	for _, file := range request.Context {
		if file.Origin != OriginSource || file.SourceCommit == "" || file.SourceCommit != sourcePins.Commit || file.StartLine > 1 || len(result.Files) != 0 {
			continue
		}
		native, err := recipe.YAMLOrJSON([]byte(file.Content))
		if err == nil && manifest.APIVersion == recipe.APIVersion && sameSourceContract(native, result.Manifest, true) {
			return nil
		}
	}
	if !upstreamManifest(result.Manifest) {
		if len(manifest.Workloads) == 0 || request.Mode == "" {
			return nil
		}
		return newError("recipe.proposal_upstream_required", "Preserve a native LMW manifest from the reviewed pinned source or propose source-owned upstream execution; do not reconstruct or repin a managed launch helper.", false)
	}
	covered := func(field string) bool {
		for _, evidence := range result.Evidence {
			p := strings.TrimPrefix(evidence.Path, "manifest.")
			if field != p {
				continue
			}
			for _, file := range request.Context {
				if file.Origin == OriginSource && file.SourceCommit == sourcePins.Commit && file.SourceCommit != "" && file.Path == evidence.SourcePath && file.SHA256 == evidence.SHA256 && evidence.SourceCommit == file.SourceCommit {
					return true
				}
			}
		}
		return false
	}
	questioned := func(field string) bool {
		for _, question := range result.Questions {
			if strings.TrimPrefix(question.Path, "manifest.") == field && strings.TrimSpace(question.Question) != "" {
				return true
			}
		}
		return false
	}
	type fact struct {
		path      string
		populated bool
		required  bool
	}
	var fields []fact
	if manifest.Metadata.Source != nil && manifest.Metadata.Source.Path != "" {
		fields = append(fields, fact{"metadata.source.path", true, false})
	}
	for i, workload := range manifest.Workloads {
		if workload.Upstream == nil {
			continue // ValidateResult rejects mixed execution ownership.
		}
		prefix := fmt.Sprintf("workloads[%d]", i)
		upstream := workload.Upstream
		fields = append(fields,
			fact{prefix + ".upstream.start", len(upstream.Start) != 0, true},
			fact{prefix + ".upstream.stop", len(upstream.Stop) != 0, true},
			fact{prefix + ".upstream.install", len(upstream.Install) != 0, false},
			fact{prefix + ".upstream.containers", len(upstream.Containers) != 0, true},
			fact{prefix + ".env", len(workload.Env) != 0, false},
			fact{prefix + ".upstream.coordinatorRank", upstream.CoordinatorRank != nil, false},
			fact{prefix + ".upstream.auxiliaryContainers", len(upstream.AuxiliaryContainers) != 0, false},
			fact{prefix + ".readiness", workload.Readiness != nil, false},
			fact{prefix + ".verify", workload.Verify != nil, false},
		)
		for rank := range upstream.InstallByRank {
			fields = append(fields, fact{fmt.Sprintf("%s.upstream.installByRank[%d]", prefix, rank), true, false})
		}
		for rank := range upstream.ContainersByRank {
			fields = append(fields, fact{fmt.Sprintf("%s.upstream.containersByRank[%d]", prefix, rank), true, false})
		}
		for _, file := range []struct{ name, value string }{{"envFile", upstream.EnvFile}, {"envTemplate", upstream.EnvTemplate}, {"envFormat", upstream.EnvFormat}, {"logFile", upstream.LogFile}} {
			fields = append(fields, fact{prefix + ".upstream." + file.name, file.value != "", false})
		}
	}
	for _, field := range fields {
		if field.populated && !covered(field.path) {
			return newError("recipe.proposal_evidence_invalid", "Populated source-owned lifecycle/configuration/observation requires an exact pinned target citation; a question cannot authorize it: "+field.path, false)
		}
		if !field.populated && field.required && !questioned(field.path) {
			return newError("recipe.proposal_question_invalid", "Leave missing upstream facts unset and ask an explicit question before requesting a new evidence-backed proposal: "+field.path, false)
		}
	}
	return nil
}

// Compare the whole authored document, including extension fields and parameter
// defaults. Typed manifests alone discard fields they do not model. Native
// imports may acquire server-owned URL/revision pins; the working directory is
// executable contract and is never discarded.
func sameSourceContract(left, right json.RawMessage, native bool) bool {
	normalize := func(raw json.RawMessage) (map[string]any, bool) {
		var document map[string]any
		if json.Unmarshal(raw, &document) != nil || document == nil {
			return nil, false
		}
		metadata, _ := document["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
			document["metadata"] = metadata
		}
		source, _ := metadata["source"].(map[string]any)
		if source == nil {
			source = map[string]any{}
			metadata["source"] = source
		}
		delete(source, "procedure")
		if native {
			delete(source, "url")
			delete(source, "revision")
		}
		if source["path"] == nil || source["path"] == "" {
			source["path"] = "."
		}
		return document, true
	}
	a, aok := normalize(left)
	b, bok := normalize(right)
	return aok && bok && reflect.DeepEqual(a, b)
}

func reviewedSourceContract(raw json.RawMessage, review *Procedure) bool {
	return review != nil && sameSourceContract(raw, review.Manifest, false)
}

// Compiler identity is never an authorization bit. Recreate the deterministic
// result from hash-verified retained source and the frozen baseline, then bind
// every manifest field to that result. This runs no upstream program.
func (s *Service) trustedCompiledContract(ctx context.Context, draft *Draft, procedure Procedure) (bool, error) {
	registry, ok := s.registry.(*repositorycompiler.Registry)
	if !ok || !upstreamManifest(procedure.Manifest) || len(procedure.Files) != 0 || len(procedure.SelectedSourceAssets) != 0 {
		return false, nil
	}
	var source GitSource
	if err := json.Unmarshal(draft.Source, &source); err != nil {
		return false, err
	}
	repositoryID, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: source.Remote, Path: source.Path})
	if err != nil {
		return false, err
	}
	repoSource := recipe.RepositorySource{RepositoryID: repositoryID, URL: source.Remote, Path: source.Path, CommitSHA: draft.ResolvedCommit, TreeSHA: draft.ResolvedTree}
	compiler, ok := registry.LookupUpstream(repoSource)
	if !ok {
		return false, nil
	}
	var previous *recipe.RecipeDetail
	if draft.ChangeContext != nil && draft.ChangeContext.BaseRecipeDigest != "" {
		base, err := recipe.Parse(draft.ChangeContext.BaseManifest)
		if err != nil {
			return false, err
		}
		previous = &recipe.RecipeDetail{Recipe: recipe.Recipe{Version: base.Metadata.Version}, Manifest: draft.ChangeContext.BaseManifest}
	}
	packed, err := compiler.CompileRetained(ctx, repoSource, func(name string) ([]byte, error) {
		candidatePath := path.Join(source.Path, name)
		if !canonicalDraftPath(candidatePath) {
			return nil, newError("recipe.proposal_evidence_invalid", "Compiler source path is not repository-relative: "+candidatePath, false)
		}
		for _, candidate := range draft.Candidates {
			if candidate.Path != candidatePath || candidate.Origin != OriginSource {
				continue
			}
			body, err := os.ReadFile(filepath.Join(s.root, draft.ID, "source", "sha256-"+candidate.SHA256))
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != candidate.SHA256 {
				return nil, newError("recipe.proposal_evidence_invalid", "Retained compiler source bytes changed: "+candidatePath, false)
			}
			return body, nil
		}
		return nil, newError("recipe.proposal_evidence_invalid", "Reviewed compiler source is not retained: "+candidatePath, false)
	}, previous)
	if err != nil {
		return false, err
	}
	if !sameSourceContract(packed.ConfigJSON, procedure.Manifest, false) {
		return false, newError("recipe.draft_review_required", "The proposal differs from the complete reviewed compiler contract; request a new evidence-backed source proposal.", false)
	}
	return true, nil
}

func unresolvedSourceFact(review *Procedure) string {
	if review == nil || !upstreamManifest(review.Manifest) {
		return ""
	}
	for _, question := range review.Questions {
		field := strings.TrimPrefix(question.Path, "manifest.")
		if field != "metadata.source.path" && !(strings.HasPrefix(field, "workloads[") && (strings.Contains(field, "].upstream.") || strings.HasSuffix(field, "].env") || strings.HasSuffix(field, "].readiness") || strings.HasSuffix(field, "].verify"))) {
			continue
		}
		covered := false
		for _, evidence := range review.Evidence {
			if strings.TrimPrefix(evidence.Path, "manifest.") == field {
				covered = true
				break
			}
		}
		if !covered {
			return field
		}
	}
	return ""
}

func reviewedAssetContract(selected []AssetSelection, review *Procedure) bool {
	if review == nil {
		return false
	}
	expected := make(map[string]string, len(review.SelectedSourceAssets)+len(review.Files))
	for _, asset := range review.SelectedSourceAssets {
		expected[asset.Path] = asset.SHA256
	}
	for _, file := range review.Files {
		sum := sha256.Sum256([]byte(file.Content))
		expected[file.Path] = hex.EncodeToString(sum[:])
	}
	if len(selected) != len(expected) {
		return false
	}
	for _, asset := range selected {
		if expected[asset.Path] != asset.SHA256 {
			return false
		}
		delete(expected, asset.Path)
	}
	return len(expected) == 0
}
