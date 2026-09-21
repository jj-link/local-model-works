package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

func foundationDraft(t *testing.T) (*Service, *Draft) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	service := New(db.New(database), root, validator, nil)
	service.SetDB(database)
	source := filepath.Join(root, "input")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "helper.sh"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateFromDir(ctx, GitSource{Remote: "https://github.com/example/repo"}, "commit", "tree", json.RawMessage(`{}`), source)
	if err != nil {
		t.Fatal(err)
	}
	return service, draft
}

func foundationGenerate(t *testing.T, service *Service, draft *Draft, result recipeassistant.Result) *Draft {
	t.Helper()
	ctx := context.Background()
	approval := generationApprovalForTest(t, service, draft, "")
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", approval, providerFunc(func(ctx context.Context, request recipeassistant.Request, _ func(string)) (recipeassistant.Result, error) {
		if request.Mode == "add" {
			source, err := request.ReadSource(ctx, draft.Candidates[0].Path)
			if err != nil {
				return recipeassistant.Result{}, err
			}
			return recipeassistant.Result{Procedures: []recipeassistant.Procedure{{
				ID: "documented", Name: "Documented procedure", Description: "Launch documented by the retained source",
				Manifest: result.Manifest, Files: result.Files, SelectedSourceAssets: result.SelectedSourceAssets, Questions: result.Questions, Adaptations: result.Adaptations,
				Evidence: []recipeassistant.Evidence{{Path: "runtime", SourcePath: source.Path, SHA256: source.SHA256, SourceCommit: source.SourceCommit, StartLine: 1, EndLine: 1}},
			}}}, nil
		}
		return result, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return generated
}

func TestProposalRemainsInertAndRejectsEditedBase(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET package_digest='saved-package' WHERE id=?`, draft.ID); err != nil {
		t.Fatal(err)
	}
	generated := foundationGenerate(t, service, draft, recipeassistant.Result{Manifest: json.RawMessage(`{"metadata":{"description":"suggested"}}`)})
	if generated.State != draft.State || generated.Operation != nil || string(generated.Manifest) != string(draft.Manifest) || generated.PackageDigest != "saved-package" {
		t.Fatalf("generation changed active work: %+v", generated)
	}
	if generated.Proposal.BaseVersion != generated.Version || len(generated.Proposal.Procedures[0].Diagnostics) == 0 {
		t.Fatalf("proposal is not reviewable: %+v", generated.Proposal)
	}
	edited, err := service.Update(ctx, generated.ID, generated.Version, UpdateRequest{Manifest: json.RawMessage(`{"metadata":{"description":"operator edit"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptProposal(ctx, edited.ID, edited.Version, generated.Proposal.ID); errorCode(err) != "recipe.proposal_stale" {
		t.Fatalf("stale acceptance: %v", err)
	}
	current, err := service.Get(ctx, edited.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Proposal == nil || string(current.Manifest) != string(edited.Manifest) {
		t.Fatal("stale suggestion or edited work was lost")
	}
}

func TestSourceCollisionSurvivesAcceptanceAndGeneratedEdit(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	original := draft.Candidates[0]
	safety := Diagnostic{ID: "safety", Code: "recipe.source_unsafe", Phase: PhaseInspect, Severity: "error", Blocking: true, Message: "unsafe source"}
	encoded, _ := marshalJSON([]Diagnostic{safety})
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET diagnostics=? WHERE id=?`, encoded, draft.ID); err != nil {
		t.Fatal(err)
	}
	generated := foundationGenerate(t, service, draft, recipeassistant.Result{Manifest: json.RawMessage(`{"assets":["helper.sh"],"metadata":{"source":{"url":"https://evil.invalid","revision":"moving"}}}`), Files: []recipeassistant.File{{Path: "helper.sh", Content: "generated\n"}}, Questions: []recipeassistant.Question{{ID: "device", Question: "Which device?", Answer: "AI chosen"}}})
	accepted, err := service.AcceptProposal(ctx, draft.ID, generated.Version, generated.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted.Questions) != 1 || accepted.Questions[0].Answer != "" {
		t.Fatalf("AI answered operator question: %+v", accepted.Questions)
	}
	var manifest struct {
		Metadata struct{ Source recipe.Source }
	}
	if err := json.Unmarshal(accepted.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.Source.URL != "https://github.com/example/repo" || manifest.Metadata.Source.Revision != "commit" {
		t.Fatalf("untrusted source accepted: %+v", manifest)
	}
	if _, body, err := service.ReadOwnedFile(ctx, draft.ID, original.Path, original.SHA256, "commit"); err != nil || string(body) != "original\n" {
		t.Fatalf("original evidence lost: %q %v", body, err)
	}
	selected, err := service.SetContext(ctx, draft.ID, accepted.Version, []ContextFile{{Path: original.Path, SHA256: original.SHA256, Origin: OriginSource}})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := service.UpdateGeneratedFile(ctx, draft.ID, selected.Version, original.Path, "edited\n")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range edited.Diagnostics {
		if diagnostic.ID == safety.ID {
			found = true
		}
	}
	if !found || edited.State != "needs_input" {
		t.Fatal("source safety was erased")
	}
	if _, body, err := service.ReadOwnedFile(ctx, draft.ID, original.Path, original.SHA256, "commit"); err != nil || string(body) != "original\n" {
		t.Fatalf("source overwritten by edit: %q %v", body, err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET state='installed' WHERE id=?`, draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateGeneratedFile(ctx, draft.ID, edited.Version, original.Path, "forbidden\n"); errorCode(err) != "recipe.draft_immutable" {
		t.Fatalf("installed edit: %v", err)
	}
	if _, body, err := service.ReadOwnedFile(ctx, draft.ID, original.Path, edited.SelectedAssets[0].SHA256, ""); err != nil || string(body) != "edited\n" {
		t.Fatalf("installed bytes changed: %q %v", body, err)
	}
}

func TestCancelledGenerationReleasesReservationWithoutReplay(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx, cancel := context.WithCancel(context.Background())
	approval := generationApprovalForTest(t, service, draft, "")
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = service.GenerateProposal(ctx, draft.ID, reserved.Operation.ID, "", approval, providerFunc(func(context.Context, recipeassistant.Request, func(string)) (recipeassistant.Result, error) {
		calls++
		cancel()
		return recipeassistant.Result{}, context.Canceled
	}), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("generation cancellation: %v", err)
	}
	current, err := service.Get(context.Background(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Operation != nil || current.State != draft.State || string(current.Manifest) != string(draft.Manifest) {
		t.Fatalf("cancel stranded draft: %+v", current)
	}
	if _, err := service.ReconcileOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("generation replayed %d times", calls)
	}
}

func TestOrphanGenerationReconcilesPreservingSuggestion(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	proposal, _ := marshalJSON(Proposal{ID: "retained", BaseVersion: draft.Version, Manifest: json.RawMessage(`{}`)})
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET state='analyzing', proposal=? WHERE id=?`, proposal, draft.ID); err != nil {
		t.Fatal(err)
	}
	count, err := service.ReconcileOperations(ctx)
	if err != nil || count != 1 {
		t.Fatalf("reconcile: %d %v", count, err)
	}
	current, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "needs_input" || current.Proposal == nil || current.Proposal.ID != "retained" || current.Operation != nil {
		t.Fatalf("orphan recovery lost work: %+v", current)
	}
	if count, err := service.ReconcileOperations(ctx); err != nil || count != 0 {
		t.Fatalf("reconcile repeated: %d %v", count, err)
	}
}

func TestContextRejectsChangedBlobAndEvidenceGaps(t *testing.T) {
	service, draft := foundationDraft(t)
	original := draft.Candidates[0]
	if err := os.Chmod(filepath.Join(service.root, draft.ID, "source", "sha256-"+original.SHA256), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(service.root, draft.ID, "source", "sha256-"+original.SHA256), []byte("corrupt!\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetContext(context.Background(), draft.ID, draft.Version, []ContextFile{{Path: original.Path, SHA256: original.SHA256}}); errorCode(err) != "recipe.draft_source_changed" {
		t.Fatalf("corrupt context: %v", err)
	}
	request := recipeassistant.Request{Context: []recipeassistant.ContextFile{{Path: "README", SHA256: "hash", SourceCommit: "old", StartLine: 1, EndLine: 2, Content: "one\ntwo\n"}, {Path: "README", SHA256: "hash", SourceCommit: "old", StartLine: 5, EndLine: 6, Content: "five\nsix\n"}}}
	result := recipeassistant.Result{Manifest: json.RawMessage(`{}`), Evidence: []recipeassistant.Evidence{{SourcePath: "README", SHA256: "hash", SourceCommit: "old", StartLine: 1, EndLine: 2}}}
	if err := validateProposalResult(result, request, nil); err != nil {
		t.Fatalf("valid first excerpt rejected: %v", err)
	}
	result.Evidence[0].EndLine = 6
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("unreviewed gap was cited")
	}
	result.Evidence[0].EndLine = 2
	result.Evidence[0].SourceCommit = "new"
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("wrong revision was cited")
	}
	request.Context = []recipeassistant.ContextFile{{Path: "README", SHA256: "hash", Content: "one\ntwo\n"}}
	result.Evidence[0] = recipeassistant.Evidence{SourcePath: "README", SHA256: "hash", StartLine: 0, EndLine: 0}
	if errorCode(validateProposalResult(result, request, nil)) != "recipe.proposal_evidence_invalid" {
		t.Fatal("zero-based whole-file citation accepted")
	}
}

func TestChangeContextSurvivesDraftCASAndSourceRevisionSelection(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	sum := sha256.Sum256([]byte("historical\n"))
	hash := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(service.root, draft.ID, "source", "sha256-"+hash), []byte("historical\n"), 0600); err != nil {
		t.Fatal(err)
	}
	change := ChangeContext{Kind: "update", RepositoryID: "repository", BaseCommit: "old", BaseRecipeDigest: "base", BaseManifest: json.RawMessage(`{"saved":true}`), BaseSourceStatus: "available", BaseCandidates: []Candidate{{Path: "helper.sh", SHA256: hash, Origin: OriginSource}}}
	encoded, _ := marshalJSON(change)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET change_context=? WHERE id=?`, encoded, draft.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := service.SetContext(ctx, draft.ID, draft.Version, []ContextFile{{Path: "helper.sh", SHA256: hash, SourceCommit: "old"}, {Path: draft.Candidates[0].Path, SHA256: draft.Candidates[0].SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.ContextSelection) != 2 || selected.ChangeContext == nil || selected.ChangeContext.BaseRecipeDigest != "base" {
		t.Fatalf("context lost revisions: %+v", selected)
	}
	edited, err := service.Update(ctx, draft.ID, selected.Version, UpdateRequest{Manifest: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	generated := foundationGenerate(t, service, edited, recipeassistant.Result{Manifest: json.RawMessage(`{}`)})
	if generated.ChangeContext == nil || string(generated.ChangeContext.BaseManifest) != `{"saved":true}` {
		t.Fatalf("operation lost baseline: %+v", generated)
	}
	listed, err := service.ListByRepository(ctx, "repository")
	if err != nil || len(listed) != 1 || listed[0].ID != draft.ID {
		t.Fatalf("repository lookup: %+v %v", listed, err)
	}
}

func TestDraftJSONKeepsEmptyCollectionsIterable(t *testing.T) {
	_, draft := foundationDraft(t)
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"candidates", "selected_assets", "diagnostics", "context_selection", "questions", "acknowledged_warnings", "resolved_references"} {
		if _, iterable := response[field].([]any); !iterable {
			t.Fatalf("draft response %s is not an array; the review client cannot iterate it: %s", field, raw)
		}
	}
}
