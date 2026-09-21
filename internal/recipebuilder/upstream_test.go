package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipe/repositorycompiler"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
	recipeassets "github.com/jj-link/local-model-works/recipes"
)

func retainApprovalSource(t *testing.T, service *Service, draft *Draft, name, body string) (*Draft, Candidate) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	candidate := Candidate{Path: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Origin: OriginSource}
	if err := storeGeneratedBlob(filepath.Join(service.root, draft.ID, "source", "sha256-"+candidate.SHA256), []byte(body)); err != nil {
		t.Fatal(err)
	}
	candidates, _ := json.Marshal(append(draft.Candidates, candidate))
	if _, err := service.db.ExecContext(context.Background(), `UPDATE recipe_drafts SET candidates=? WHERE id=?`, string(candidates), draft.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Get(context.Background(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated, candidate
}

func TestUpstreamOverrideFactsRequireTheirOwnEvidence(t *testing.T) {
	cases := []struct{ name, field, fragment string }{
		{"rank install", "workloads[0].upstream.installByRank[1]", `"installByRank":{"1":[["./install-worker.sh"]]}`},
		{"empty rank install", "workloads[0].upstream.installByRank[1]", `"installByRank":{"1":[]}`},
		{"coordinator zero", "workloads[0].upstream.coordinatorRank", `"coordinatorRank":0`},
		{"rank containers", "workloads[0].upstream.containersByRank[1]", `"containersByRank":{"1":["worker"]}`},
		{"auxiliary containers", "workloads[0].upstream.auxiliaryContainers", `"auxiliaryContainers":["cache"]`},
		{"environment format", "workloads[0].upstream.envFormat", `"envFormat":"shell"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := recipeassistant.Request{Mode: "update", SourcePins: map[string]any{"commit": "target"}, Context: []recipeassistant.ContextFile{{Path: "README.md", SHA256: "hash", Origin: OriginSource, SourceCommit: "target", Content: "source lifecycle\n"}}}
			result := recipeassistant.Result{Manifest: json.RawMessage(`{"workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["head"],` + tc.fragment + `}}]}`)}
			for _, field := range []string{"workloads[0].upstream.start", "workloads[0].upstream.stop", "workloads[0].upstream.containers"} {
				result.Evidence = append(result.Evidence, recipeassistant.Evidence{Path: field, SourcePath: "README.md", SHA256: "hash", SourceCommit: "target", StartLine: 1, EndLine: 1})
			}
			result.Questions = []recipeassistant.Question{{ID: "override", Path: tc.field, Question: "What is the supported override?"}}
			if err := validateProposalResult(result, request, nil); errorCode(err) != "recipe.proposal_evidence_invalid" || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("uncited override escaped precise provenance gate: %v", err)
			}
			result.Evidence = append(result.Evidence, recipeassistant.Evidence{Path: tc.field, SourcePath: "README.md", SHA256: "hash", SourceCommit: "target", StartLine: 1, EndLine: 1})
			if err := validateProposalResult(result, request, nil); err != nil {
				t.Fatalf("exact override citation rejected: %v", err)
			}
		})
	}
}

func TestNativeProposalPreservesEntireAuthoredManifest(t *testing.T) {
	authored := json.RawMessage(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"native"},"settings":{"port":{"default":8000}},"compatibility":{"nodeCount":2},"variants":[{"name":"tp2"}],"workloads":[{"command":["serve","${setting.port}"]}]}`)
	request := recipeassistant.Request{Mode: "update", SourcePins: map[string]any{"commit": "target"}, Context: []recipeassistant.ContextFile{{Path: "recipe.json", SHA256: "hash", Origin: OriginSource, SourceCommit: "target", Content: string(authored)}}}
	for _, tc := range []struct{ name, before, after string }{
		{"setting default", "8000", "9000"},
		{"topology", `"nodeCount":2`, `"nodeCount":1`},
		{"variant", `"tp2"`, `"other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := recipeassistant.Result{Manifest: json.RawMessage(strings.Replace(string(authored), tc.before, tc.after, 1))}
			if err := validateSourceOwnedProposal(result, request); errorCode(err) != "recipe.proposal_upstream_required" {
				t.Fatalf("altered native contract accepted: %v", err)
			}
		})
	}
	pinned := strings.Replace(string(authored), `"name":"native"`, `"name":"native","source":{"url":"https://github.com/example/native","revision":"target","path":".","procedure":"native"}`, 1)
	if err := validateSourceOwnedProposal(recipeassistant.Result{Manifest: json.RawMessage(pinned)}, request); err != nil {
		t.Fatalf("unchanged native source with server pins rejected: %v", err)
	}
	changedPath := strings.Replace(pinned, `"path":"."`, `"path":"unreviewed"`, 1)
	if err := validateSourceOwnedProposal(recipeassistant.Result{Manifest: json.RawMessage(changedPath)}, request); errorCode(err) != "recipe.proposal_upstream_required" {
		t.Fatalf("native working directory edit accepted: %v", err)
	}
}

func TestAcceptedReviewBlocksExecutableEditsButAllowsAnswers(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	manifest := json.RawMessage(`{"metadata":{"source":{"url":"https://github.com/example/repo","revision":"commit","path":"."}},"compatibility":{"nodeCount":2},"parameters":[{"name":"port","default":8000}],"workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["head"]}}]}`)
	change := ChangeContext{Kind: "add", Review: &Procedure{Manifest: manifest}}
	encoded, _ := json.Marshal(change)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET manifest=?, change_context=?, questions=? WHERE id=?`, string(manifest), string(encoded), `[{"id":"supported","question":"Use the documented configuration?"}]`, draft.ID); err != nil {
		t.Fatal(err)
	}
	draft, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct{ before, after string }{
		{"./start.sh", "./invented.sh"},
		{`"path":"."`, `"path":"elsewhere"`},
		{`"nodeCount":2`, `"nodeCount":1`},
		{"8000", "9000"},
	} {
		changed := json.RawMessage(strings.Replace(string(manifest), edit.before, edit.after, 1))
		if _, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{Manifest: changed}); errorCode(err) != "recipe.draft_review_required" {
			t.Fatalf("post-review edit authorized: %v", err)
		}
		stale := *draft
		stale.Manifest = changed
		if err := service.checkReadiness(ctx, &stale); errorCode(err) != "recipe.draft_review_required" {
			t.Fatalf("persisted stale review authorized packaging: %v", err)
		}
	}
	answered, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{Manifest: manifest, Answers: []Answer{{QuestionID: "supported", Answer: "yes"}}})
	if err != nil || answered.Questions[0].Answer != "yes" || !jsonEqual(answered.Manifest, manifest) {
		t.Fatalf("answer-only flow changed or lost reviewed contract: %+v %v", answered, err)
	}
	if err := service.checkReadiness(ctx, answered); errorCode(err) != "recipe.proposal_evidence_invalid" {
		t.Fatalf("old evidence-free review plus answers authorized execution: %v", err)
	}
}

func TestUpdateWithoutCompilerCannotRepinSavedHelper(t *testing.T) {
	service, saved := savedChangeFixture(t)
	ctx := context.Background()
	draft, err := service.AllocateChange(ctx, saved.Digest, ChangeRequest{Kind: "update", ExpectedCurrentDigest: saved.Digest, ExpectedHeadCommit: strings.Repeat("b", 40)})
	if err != nil {
		t.Fatal(err)
	}
	registry := repositorycompiler.NewRegistry(service.validator)
	service.SetRepositoryCompilerRegistry(registry)
	if _, ok := registry.Lookup(recipe.RepositorySource{RepositoryID: draft.ChangeContext.RepositoryID, URL: "https://github.com/example/unavailable-history", Path: "."}, filepath.Join(service.root, draft.ID, "source")); ok {
		t.Fatal("fixture unexpectedly has a compiler")
	}
	updated, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{Manifest: draft.Manifest, SelectedAssets: draft.SelectedAssets})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.checkChangeBaseline(ctx, updated); errorCode(err) != "recipe.proposal_upstream_required" {
		t.Fatalf("no compiler/review authorized stale-helper repinning: %v", err)
	}
}

func TestCompilerAcceptanceReconstructsExactReviewedProcedure(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	template, err := recipeassets.Templates.ReadFile("qwen38-27b-dgx-spark-mtp/recipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document, err := recipe.YAMLOrJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	source := manifest.Metadata.Source
	repositoryID, _, _, err := recipe.RepositoryIdentity(*source)
	if err != nil {
		t.Fatal(err)
	}
	repoSource := recipe.RepositorySource{RepositoryID: repositoryID, URL: source.URL, Path: source.Path, CommitSHA: source.Revision}
	registry := repositorycompiler.NewRegistry(service.validator)
	service.SetRepositoryCompilerRegistry(registry)
	compiler, ok := registry.LookupUpstream(repoSource)
	if !ok {
		t.Fatal("reviewed procedure is not registered")
	}
	// The deterministic compiler requests only authored source files. Retain
	// those fixture bytes as the inspection ledger would, without executing any.
	required := map[string]string{}
	packed, err := compiler.CompileRetained(ctx, repoSource, func(name string) ([]byte, error) {
		body, err := os.ReadFile(filepath.Join("..", "recipe", "repositorycompiler", "testdata", "qwen38-27b-dgx-spark-mtp", name))
		if err != nil {
			return nil, err
		}
		required[name] = string(body)
		return body, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sourceJSON, _ := json.Marshal(GitSource{Remote: source.URL, Path: source.Path})
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET source=?, resolved_commit=? WHERE id=?`, string(sourceJSON), source.Revision, draft.ID); err != nil {
		t.Fatal(err)
	}
	draft, err = service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range required {
		draft, _ = retainApprovalSource(t, service, draft, name, body)
	}
	proposal := Proposal{ID: "trusted-compiler", ProviderID: "compiler", BaseVersion: draft.Version, Manifest: packed.ConfigJSON}
	for _, change := range []struct{ before, after string }{
		{manifest.Workloads[0].Upstream.Start[0], "./unreviewed-start.sh"},
		{`"nodeCount":1`, `"nodeCount":2`},
	} {
		altered := proposal
		altered.Manifest = json.RawMessage(strings.Replace(string(packed.ConfigJSON), change.before, change.after, 1))
		if jsonEqual(altered.Manifest, packed.ConfigJSON) {
			t.Fatalf("fixture edit did not alter the contract: %q", change.before)
		}
		encoded, _ := json.Marshal(altered)
		if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET proposal=? WHERE id=?`, string(encoded), draft.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := service.AcceptProposal(ctx, draft.ID, draft.Version, proposal.ID); errorCode(err) != "recipe.draft_review_required" {
			t.Fatalf("compiler label authorized altered execution: %v", err)
		}
	}
	encoded, _ := json.Marshal(proposal)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET proposal=? WHERE id=?`, string(encoded), draft.ID); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.AcceptProposal(ctx, draft.ID, draft.Version, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reviewedSourceContract(accepted.Manifest, accepted.Review) {
		t.Fatal("exact compiler contract was not retained in the accepted review")
	}
	if err := service.checkReadiness(ctx, accepted); err != nil {
		t.Fatalf("exact retained procedure did not remain packageable: %v", err)
	}
}

func TestSourceWorkingDirectoryNeedsExactCitation(t *testing.T) {
	request := recipeassistant.Request{Mode: "update", SourcePins: map[string]any{"commit": "target"}, Context: []recipeassistant.ContextFile{{Path: "README.md", SHA256: "hash", Origin: OriginSource, SourceCommit: "target", Content: "Run scripts from the deployment directory\n"}}}
	result := recipeassistant.Result{Manifest: json.RawMessage(`{"metadata":{"source":{"path":"deployment"}},"workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["head"]}}]}`)}
	for _, field := range []string{"metadata.source", "workloads[0].upstream.start", "workloads[0].upstream.stop", "workloads[0].upstream.containers"} {
		result.Evidence = append(result.Evidence, recipeassistant.Evidence{Path: field, SourcePath: "README.md", SHA256: "hash", SourceCommit: "target", StartLine: 1, EndLine: 1})
	}
	if err := validateProposalResult(result, request, nil); errorCode(err) != "recipe.proposal_evidence_invalid" || !strings.Contains(err.Error(), "metadata.source.path") {
		t.Fatalf("source ancestor citation authorized the working directory: %v", err)
	}
	result.Evidence[0].Path = "metadata.source.path"
	if err := validateProposalResult(result, request, nil); err != nil {
		t.Fatalf("exact source working directory citation rejected: %v", err)
	}
}

func TestNativeReviewBindsSelectedSourceBytes(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	original := draft.Candidates[0]
	draft, alternate := retainApprovalSource(t, service, draft, original.Path, "unreviewed executable bytes\n")
	manifest := json.RawMessage(`{"metadata":{"source":{"url":"https://github.com/example/repo","revision":"commit","path":"."}},"workloads":[{"command":["/lmw/assets/helper.sh"]}],"assets":["helper.sh"]}`)
	selected := []AssetSelection{{Path: original.Path, SHA256: original.SHA256, Origin: OriginSource}}
	changeJSON, _ := json.Marshal(ChangeContext{Kind: "add", Review: &Procedure{Manifest: manifest, SelectedSourceAssets: selected}})
	selectedJSON, _ := json.Marshal(selected)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET manifest=?, change_context=?, selected_assets=? WHERE id=?`, string(manifest), string(changeJSON), string(selectedJSON), draft.ID); err != nil {
		t.Fatal(err)
	}
	draft, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []AssetSelection{{Path: alternate.Path, SHA256: alternate.SHA256, Origin: OriginSource}}
	if _, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{Manifest: manifest, SelectedAssets: replacement}); errorCode(err) != "recipe.draft_review_required" {
		t.Fatalf("unchanged manifest authorized different executable asset bytes: %v", err)
	}
	draft.SelectedAssets = replacement
	if err := service.checkReadiness(ctx, draft); errorCode(err) != "recipe.draft_review_required" {
		t.Fatalf("persisted asset substitution escaped packaging: %v", err)
	}
}
