package recipebuilder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

type providerFunc func(context.Context, recipeassistant.Request, func(string)) (recipeassistant.Result, error)

func (f providerFunc) Generate(ctx context.Context, request recipeassistant.Request, progress func(string)) (recipeassistant.Result, error) {
	return f(ctx, request, progress)
}

func generationApprovalForTest(t *testing.T, service *Service, draft *Draft, instruction string) GenerationApproval {
	t.Helper()
	request, err := service.GenerationRequest(context.Background(), draft.ID, draft.Version, instruction, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	approval := GenerationApproval{DraftVersion: draft.Version, ProviderID: "provider", ProviderVersion: "settings-v4", Destination: "https://provider.invalid/v1", ProviderKind: "openai_compatible", Request: request}
	approval.PreviewSHA256, err = approval.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func TestGenerateProposalBindsDraftAndProviderVersions(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	service := New(db.New(database), state, validator, nil)
	service.SetDB(database)
	source := filepath.Join(state, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("reviewed context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateFromDir(ctx, map[string]string{"remote": "https://github.com/example/repository"}, "commit", "tree", json.RawMessage(`{}`), source)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	provider := providerFunc(func(ctx context.Context, request recipeassistant.Request, _ func(string)) (recipeassistant.Result, error) {
		called = true
		source, err := request.ReadSource(ctx, "README.md")
		if err != nil {
			return recipeassistant.Result{}, err
		}
		return recipeassistant.Result{Procedures: []recipeassistant.Procedure{{
			ID: "documented", Name: "Documented launch", Description: "The retained documented launch procedure",
			Manifest: json.RawMessage(`{}`), Files: []recipeassistant.File{{Path: "generated/start.sh", Content: "echo safe\n"}},
			Evidence: []recipeassistant.Evidence{{Path: "runtime", SourcePath: source.Path, SHA256: source.SHA256, SourceCommit: source.SourceCommit, StartLine: 1, EndLine: 1}},
		}}}, nil
	})
	if _, err := service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", GenerationApproval{DraftVersion: draft.Version}, provider, nil); errorCode(err) != "recipe.generation_consent_stale" {
		t.Fatalf("stale consent error = %v", err)
	}
	if called {
		t.Fatal("provider ran after stale consent")
	}
	failed, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	approval := generationApprovalForTest(t, service, failed, "correct")
	reserved, err = service.ReserveOperation(ctx, draft.ID, failed.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", approval, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !called || generated.Proposal == nil || generated.Proposal.BaseVersion != generated.Version || generated.Proposal.ProviderVersion != "settings-v4" || generated.State != failed.State || generated.Operation != nil {
		t.Fatalf("generated draft = %+v", generated)
	}
	if _, err := service.AcceptProposal(ctx, generated.ID, generated.Version-1, generated.Proposal.ID); errorCode(err) != "recipe.draft_stale_version" {
		t.Fatalf("stale accept error = %v", err)
	}
	if _, err := service.AcceptProposal(ctx, generated.ID, generated.Version, "other-proposal"); errorCode(err) != "recipe.proposal_stale" {
		t.Fatalf("wrong proposal accept error = %v", err)
	}
	accepted, err := service.AcceptProposal(ctx, generated.ID, generated.Version, generated.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Proposal != nil {
		t.Fatal("accepted proposal was retained")
	}
	foundGenerated := false
	for _, candidate := range accepted.Candidates {
		if candidate.Path == "generated/start.sh" && candidate.Origin == OriginGenerated {
			foundGenerated = true
			if body, readErr := os.ReadFile(filepath.Join(state, "drafts", accepted.ID, "source", "sha256-"+candidate.SHA256)); readErr != nil || string(body) != "echo safe\n" {
				t.Fatalf("generated blob body=%q err=%v", body, readErr)
			}
		}
	}
	if !foundGenerated {
		t.Fatalf("generated candidate missing: %+v", accepted.Candidates)
	}
	candidate, body, err := service.ReadOwnedFile(ctx, accepted.ID, "generated/start.sh", accepted.SelectedAssets[0].SHA256, "")
	if err != nil || candidate.Origin != OriginGenerated || string(body) != "echo safe\n" {
		t.Fatalf("owned file candidate=%+v body=%q err=%v", candidate, body, err)
	}
	if _, _, err := service.ReadOwnedFile(ctx, accepted.ID, "../lmw.db", "", ""); errorCode(err) != "recipe.draft_file_invalid" {
		t.Fatalf("unsafe owned file error = %v", err)
	}
	edited, err := service.UpdateGeneratedFile(ctx, accepted.ID, accepted.Version, "generated/start.sh", "echo edited\n")
	if err != nil {
		t.Fatal(err)
	}
	if edited.PackageDigest != "" {
		t.Fatalf("edited draft retained package digest: %+v", edited)
	}
	_, editedBody, err := service.ReadOwnedFile(ctx, edited.ID, "generated/start.sh", edited.SelectedAssets[0].SHA256, "")
	if err != nil || string(editedBody) != "echo edited\n" {
		t.Fatalf("edited body=%q err=%v", editedBody, err)
	}
	accepted = edited
	approval = generationApprovalForTest(t, service, accepted, "")
	reserved, err = service.ReserveOperation(ctx, accepted.ID, accepted.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	regenerated, err := service.GenerateProposal(ctx, accepted.ID, reserved.Operation.ID, "", approval, provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := service.DiscardProposal(ctx, regenerated.ID, regenerated.Version, regenerated.Proposal.ID)
	if err != nil || discarded.Proposal != nil || discarded.Manifest == nil {
		t.Fatalf("discarded=%+v err=%v", discarded, err)
	}
}

func TestValidateProposalResultRejectsUnverifiedAssetsAndEvidence(t *testing.T) {
	request := recipeassistant.Request{Context: []recipeassistant.ContextFile{{
		Path: "README.md", SHA256: "source-hash", StartLine: 2, EndLine: 4, Content: "two\nthree\nfour\n",
	}}}
	candidates := []Candidate{{Path: "weights.bin", SHA256: "asset-hash", Origin: OriginSource}}
	base := recipeassistant.Result{Manifest: json.RawMessage(`{}`)}
	badAsset := base
	badAsset.SelectedSourceAssets = []recipeassistant.AssetSelection{{Path: "weights.bin", SHA256: "wrong"}}
	if code := errorCode(validateProposalResult(badAsset, request, candidates)); code != "recipe.proposal_asset_invalid" {
		t.Fatalf("bad asset error code = %q", code)
	}
	badEvidence := base
	badEvidence.Evidence = []recipeassistant.Evidence{{Path: "manifest.runtime", SourcePath: "README.md", StartLine: 1, EndLine: 3}}
	if code := errorCode(validateProposalResult(badEvidence, request, candidates)); code != "recipe.proposal_evidence_invalid" {
		t.Fatalf("bad evidence error code = %q", code)
	}
}

func TestApprovedGenerationRejectsChangedSourceBeforeProviderCall(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	candidate := draft.Candidates[0]
	selected, err := service.SetContext(ctx, draft.ID, draft.Version, []ContextFile{{Path: candidate.Path, SHA256: candidate.SHA256, Origin: candidate.Origin}})
	if err != nil {
		t.Fatal(err)
	}
	approval := generationApprovalForTest(t, service, selected, "")
	reserved, err := service.ReserveOperation(ctx, selected.ID, selected.Version, PhaseGenerate)
	if err := os.Chmod(filepath.Join(service.root, draft.ID, "source", "sha256-"+candidate.SHA256), 0600); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(service.root, draft.ID, "source", "sha256-"+candidate.SHA256), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", approval, providerFunc(func(context.Context, recipeassistant.Request, func(string)) (recipeassistant.Result, error) {
		calls++
		return recipeassistant.Result{}, nil
	}), nil)
	if err == nil || calls != 0 {
		t.Fatalf("changed source reached provider: calls=%d err=%v", calls, err)
	}
	current, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Operation != nil || current.State == "analyzing" {
		t.Fatalf("corrupt evidence stranded work: %+v", current)
	}
}

func TestProcedureAcceptanceSeparatesDraftsAndReplaysWithoutDuplicates(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	approval := generationApprovalForTest(t, service, draft, "")
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", approval, providerFunc(func(ctx context.Context, request recipeassistant.Request, _ func(string)) (recipeassistant.Result, error) {
		source, err := request.ReadSource(ctx, "helper.sh")
		if err != nil {
			return recipeassistant.Result{}, err
		}
		procedure := recipeassistant.Procedure{ID: "single", Name: "Single node", Description: "Documented single-node launch", Manifest: json.RawMessage(`{"assets":["helper.sh"]}`),
			Files:    []recipeassistant.File{{Path: source.Path, Content: source.Content}},
			Evidence: []recipeassistant.Evidence{{Path: "runtime", SourcePath: source.Path, SHA256: source.SHA256, SourceCommit: source.SourceCommit, StartLine: 1, EndLine: 1}}}
		second := procedure
		second.ID, second.Name, second.Description = "cluster", "Cluster", "Documented cluster launch"
		second.Questions = []recipeassistant.Question{{ID: "topology", Path: "compatibility.nodeCount", Question: "How many hosts does your cluster have?"}}
		return recipeassistant.Result{Procedures: []recipeassistant.Procedure{procedure, second}}, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.AcceptProposal(ctx, draft.ID, generated.Version, generated.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted.RelatedDraftIDs) != 1 || accepted.Review == nil || accepted.Review.ID != "single" {
		t.Fatalf("missing accepted procedure siblings: %+v", accepted)
	}
	sibling, err := service.Get(ctx, accepted.RelatedDraftIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Review.ID != "cluster" || sibling.ParentDraftID != draft.ID || sibling.State != "needs_input" || len(sibling.Questions) != 1 {
		t.Fatalf("sibling lost its independent review/questions: %+v", sibling)
	}
	for _, item := range []*Draft{accepted, sibling} {
		if len(item.SelectedAssets) != 1 || item.SelectedAssets[0].Origin != OriginSource {
			t.Fatalf("unchanged upstream helper was rewritten: %+v", item.SelectedAssets)
		}
		if _, body, err := service.ReadOwnedFile(ctx, item.ID, "helper.sh", item.SelectedAssets[0].SHA256, ""); err != nil || string(body) != "original\n" {
			t.Fatalf("source helper not retained: %q %v", body, err)
		}
	}
	firstManifest, _ := recipe.Parse(accepted.Manifest)
	secondManifest, _ := recipe.Parse(sibling.Manifest)
	firstID, _, _, _ := recipe.RepositoryIdentity(*firstManifest.Metadata.Source)
	secondID, _, _, _ := recipe.RepositoryIdentity(*secondManifest.Metadata.Source)
	if firstID == secondID {
		t.Fatal("distinct procedures collide in the catalog")
	}
	replayed, err := service.AcceptProposal(ctx, draft.ID, generated.Version, generated.Proposal.ID)
	if err != nil || len(replayed.RelatedDraftIDs) != 1 || replayed.RelatedDraftIDs[0] != sibling.ID {
		t.Fatalf("acceptance replay duplicated or lost siblings: %+v %v", replayed, err)
	}
	updated, err := service.Update(ctx, sibling.ID, sibling.Version, UpdateRequest{Manifest: sibling.Manifest, SelectedAssets: sibling.SelectedAssets, Answers: []Answer{{QuestionID: "topology", Answer: "2"}}})
	if err != nil || updated.Questions[0].Answer != "2" {
		t.Fatalf("independent answer failed: %+v %v", updated, err)
	}
	unchanged, err := service.Get(ctx, accepted.ID)
	if err != nil || unchanged.Version != accepted.Version || len(unchanged.Questions) != 0 {
		t.Fatalf("sibling edit mutated first procedure: %+v %v", unchanged, err)
	}
}

func TestUpstreamProposalRequiresExactTargetEvidence(t *testing.T) {
	manifest := json.RawMessage(`{"workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["upstream"]}}]}`)
	result := recipeassistant.Result{Manifest: manifest}
	request := recipeassistant.Request{Mode: "update", SourcePins: map[string]any{"commit": "new"}, Context: []recipeassistant.ContextFile{{Path: "README.md", SHA256: "hash", Origin: OriginSource, SourceCommit: "new", Content: "./start.sh starts upstream; ./stop.sh stops it\n"}}}
	for _, field := range []string{"start", "stop", "containers"} {
		result.Evidence = append(result.Evidence, recipeassistant.Evidence{Path: "workloads[0].upstream." + field, SourcePath: "README.md", SHA256: "hash", SourceCommit: "new", StartLine: 1, EndLine: 1})
	}
	if err := validateProposalResult(result, request, nil); err != nil {
		t.Fatalf("exact target citations rejected: %v", err)
	}
	result.Evidence[0].SourceCommit = "old"
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("old revision authorized the target command")
	}
	result.Evidence[0].SourceCommit = "new"
	result.Evidence[0].Path = "workloads[0]"
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("ancestor citation authorized an uncited command")
	}
	result.Questions = []recipeassistant.Question{{ID: "start", Path: "workloads[0].upstream.start", Question: "Which upstream start command?", Answer: "./start.sh"}}
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("answered question authorized a prepopulated command")
	}
	result.Manifest = json.RawMessage(`{"workloads":[{"upstream":{"stop":["./stop.sh"],"containers":["upstream"]}}]}`)
	if err := validateProposalResult(result, request, nil); err != nil {
		t.Fatalf("unset fact with explicit question rejected: %v", err)
	}
	result.Questions = nil
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_question_invalid" {
		t.Fatal("missing command escaped the question gate")
	}
}

func TestExistingRecipePreviewIncludesPinnedSourceAndRefinement(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	change := ChangeContext{Kind: "repair", BaseRecipeDigest: "saved", BaseCommit: draft.ResolvedCommit, BaseManifest: draft.Manifest, BaseSourceStatus: "available"}
	changeJSON, _ := json.Marshal(change)
	pending := Proposal{ID: "pending", Manifest: json.RawMessage(`{"metadata":{"description":"refine this suggestion"}}`), Questions: []Question{{ID: "start", Path: "workloads[0].upstream.start", Question: "Which documented start procedure?"}}, Diagnostics: []Diagnostic{{ID: "private", Message: "unselected diagnostic"}}}
	proposalJSON, _ := json.Marshal(pending)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET change_context=?, proposal=?, questions=? WHERE id=?`, string(changeJSON), string(proposalJSON), `[{"id":"stop","question":"Which stop procedure?","answer":"Use documented stop.sh"}]`, draft.ID); err != nil {
		t.Fatal(err)
	}
	request, err := service.GenerationRequest(ctx, draft.ID, draft.Version, "refine", "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.Mode != "repair" || len(request.Context) != 1 || request.Context[0].Content != "original\n" || request.Context[0].SourceCommit != draft.ResolvedCommit {
		t.Fatalf("existing recipe request lost pinned source: %+v", request)
	}
	var refinement recipeassistant.Result
	if err := json.Unmarshal(request.PendingProposal, &refinement); err != nil || string(refinement.Manifest) != string(pending.Manifest) || len(refinement.Questions) != 1 {
		t.Fatalf("pending suggestion unavailable for refinement: %s %v", request.PendingProposal, err)
	}
	if len(request.Questions) != 1 || request.Questions[0].Answer != "Use documented stop.sh" || len(request.Diagnostics) != 0 || len(request.RunExcerpts) != 0 {
		t.Fatalf("answers lost or private diagnostics/logs attached: %+v", request)
	}
	approval := GenerationApproval{Request: request}
	before, err := approval.Digest()
	if err != nil {
		t.Fatal(err)
	}
	approval.Request.PendingProposal = json.RawMessage(`{"manifest":{}}`)
	after, err := approval.Digest()
	if err != nil || before == after {
		t.Fatal("refinement content escaped consent digest")
	}
}
