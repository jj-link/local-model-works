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
	provider := providerFunc(func(context.Context, recipeassistant.Request, func(string)) (recipeassistant.Result, error) {
		called = true
		return recipeassistant.Result{Manifest: json.RawMessage(`{}`), Files: []recipeassistant.File{{Path: "generated/start.sh", Content: "echo safe\n"}}}, nil
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
