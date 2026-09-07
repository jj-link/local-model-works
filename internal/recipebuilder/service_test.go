package recipebuilder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestDraftPersistsHashedCandidatesAndEnforcesOperationCAS(t *testing.T) {
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
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "start.sh"), []byte("docker run --privileged image\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "asset.txt"), []byte("asset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("asset.txt", filepath.Join(source, "link")); err != nil {
		t.Skip(err)
	}
	draft, err := service.CreateFromDir(ctx, map[string]string{"type": "git"}, strings.Repeat("a", 40), strings.Repeat("b", 40), nil, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Candidates) != 2 {
		t.Fatalf("draft = %+v", draft)
	}
	codes := map[string]bool{}
	for _, diagnostic := range draft.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes["recipe.draft_symlink"] || !codes["recipe.draft_host_lifecycle"] {
		t.Fatalf("diagnostics = %+v", draft.Diagnostics)
	}
	for _, candidate := range draft.Candidates {
		stored := filepath.Join(state, "drafts", draft.ID, "source", "sha256-"+candidate.SHA256)
		info, statErr := os.Stat(stored)
		if statErr != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("candidate %s mode=%v err=%v", candidate.Path, info.Mode().Perm(), statErr)
		}
	}
	var warningAck string
	for _, diagnostic := range draft.Diagnostics {
		if diagnostic.Code == "recipe.draft_host_lifecycle" {
			warningAck = warningHash(diagnostic)
			break
		}
	}
	if warningAck == "" {
		t.Fatal("host lifecycle warning hash is missing")
	}
	manifest, _ := json.Marshal(map[string]any{"invalid": true})
	selected := []AssetSelection{{
		Path: draft.Candidates[0].Path, SHA256: draft.Candidates[0].SHA256,
	}}
	invalidAcks := []string{"not-a-current-warning"}
	if _, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{
		Manifest: manifest, SelectedAssets: selected, AcknowledgedWarnings: &invalidAcks,
	}); errorCode(err) != "recipe.warning_ack_invalid" {
		t.Fatalf("invalid acknowledgement error = %v", err)
	}
	acks := []string{warningAck, warningAck}
	updated, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{
		Manifest: manifest, SelectedAssets: selected, AcknowledgedWarnings: &acks,
	})
	if err != nil || updated.Version != draft.Version+1 || updated.State != "needs_input" ||
		len(updated.AcknowledgedWarnings) != 1 || updated.AcknowledgedWarnings[0] != warningAck {
		t.Fatalf("updated = %+v, err=%v", updated, err)
	}
	emptyAcks := []string{}
	cleared, err := service.Update(ctx, draft.ID, updated.Version, UpdateRequest{
		Manifest: manifest, SelectedAssets: selected, AcknowledgedWarnings: &emptyAcks,
	})
	if err != nil || len(cleared.AcknowledgedWarnings) != 0 {
		t.Fatalf("cleared = %+v, err=%v", cleared, err)
	}
	if _, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{
		Manifest: manifest, SelectedAssets: selected,
	}); errorCode(err) != "recipe.draft_stale_version" {
		t.Fatalf("stale update error = %v", err)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, cleared.Version, PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.State != "analyzing" || reserved.Operation == nil || reserved.Operation.Kind != PhaseGenerate {
		t.Fatalf("reserved = %+v", reserved)
	}
	if _, err := service.Update(ctx, draft.ID, reserved.Version, UpdateRequest{
		Manifest: manifest, SelectedAssets: selected,
	}); errorCode(err) != "recipe.draft_operation_active" {
		t.Fatalf("update during operation = %v", err)
	}
	if err := service.Delete(ctx, draft.ID, reserved.Version); errorCode(err) != "recipe.draft_operation_active" {
		t.Fatalf("delete during operation = %v", err)
	}
	_, _ = service.FailOperation(ctx, draft.ID, reserved.Operation.ID,
		newError("recipe.test_failure", "synthetic operation failure", false))
	terminal, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != cleared.State || terminal.Operation != nil || terminal.Version != reserved.Version+1 {
		t.Fatalf("terminal = %+v", terminal)
	}
	var attemptID, validatorID string
	for _, diagnostic := range terminal.Diagnostics {
		if diagnostic.Code == "recipe.test_failure" {
			attemptID = diagnostic.ID
		}
		if diagnostic.Phase == PhaseValidate {
			validatorID = diagnostic.ID
		}
	}
	if validatorID != "" {
		if _, err := service.DismissDiagnostics(ctx, draft.ID, terminal.Version, []string{validatorID}); errorCode(err) != "recipe.diagnostic_not_dismissible" {
			t.Fatalf("validator dismissal error = %v", err)
		}
	}
	if attemptID == "" {
		t.Fatalf("attempt diagnostic missing: %+v", terminal.Diagnostics)
	}
	terminal, err = service.DismissDiagnostics(ctx, draft.ID, terminal.Version, []string{attemptID})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, draft.ID, updated.Version); errorCode(err) != "recipe.draft_stale_version" {
		t.Fatalf("stale delete error = %v", err)
	}
	if err := service.Delete(ctx, draft.ID, terminal.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "drafts", draft.ID)); !os.IsNotExist(err) {
		t.Fatalf("draft source remains after delete: %v", err)
	}
}

func TestOperationFailurePreservesPinnedSourceEvidence(t *testing.T) {
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
	draft, err := service.Allocate(ctx, GitSource{
		Remote: "https://github.com/example/repository.git/", Revision: "Release/RC1", Path: ".",
	})
	if err != nil {
		t.Fatal(err)
	}
	var storedSource GitSource
	if err := json.Unmarshal(draft.Source, &storedSource); err != nil {
		t.Fatal(err)
	}
	if storedSource.Remote != "https://github.com/example/repository" ||
		storedSource.Revision != "Release/RC1" || storedSource.Path != "" {
		t.Fatalf("stored source was not normalized without changing ref case: %+v", storedSource)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseInspect)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("c", 40)
	tree := strings.Repeat("d", 40)
	rows, err := service.q.PinRecipeDraftSource(ctx, db.PinRecipeDraftSourceParams{
		ResolvedCommit: nullable(commit), ResolvedTree: nullable(tree),
		ID: draft.ID, Operation: nullable(reserved.Operation.ID), ResolvedCommit_2: nullable(commit),
	})
	if err != nil || rows != 1 {
		t.Fatalf("pin rows=%d err=%v", rows, err)
	}
	_, _ = service.FailOperation(ctx, draft.ID, reserved.Operation.ID,
		newError("recipe.test_failure", "synthetic operation failure", true))
	failed, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ResolvedCommit != commit || failed.ResolvedTree != tree || failed.State != "failed" ||
		failed.Operation != nil {
		t.Fatalf("failed draft lost terminal evidence: %+v", failed)
	}
}

func TestReconcileOperationsMarksInterruptedWithoutReplay(t *testing.T) {
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
	draft, err := service.Allocate(ctx, GitSource{Remote: "https://github.com/example/repository"})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseInspect)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Operation == nil {
		t.Fatal("operation was not reserved")
	}
	count, err := service.ReconcileOperations(ctx)
	if err != nil || count != 1 {
		t.Fatalf("reconcile count=%d err=%v", count, err)
	}
	reconciled, err := service.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.State != "failed" || reconciled.Operation != nil {
		t.Fatalf("reconciled draft = %+v", reconciled)
	}
	found := false
	for _, diagnostic := range reconciled.Diagnostics {
		if diagnostic.Code == "recipe.operation_interrupted" && diagnostic.Retryable {
			found = true
		}
	}

	if !found {
		t.Fatalf("interruption diagnostic missing: %+v", reconciled.Diagnostics)
	}
	count, err = service.ReconcileOperations(ctx)
	if err != nil || count != 0 {
		t.Fatalf("second reconcile replayed work: count=%d err=%v", count, err)
	}
}
func TestContextSelectionMergesRangesAndEnforcesUTF8Budgets(t *testing.T) {
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
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "text.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "large.txt"), []byte(strings.Repeat("x", maxContextFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "binary.bin"), []byte{0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateFromDir(ctx, map[string]string{"type": "git"},
		strings.Repeat("a", 40), strings.Repeat("b", 40), nil, source)
	if err != nil {
		t.Fatal(err)
	}
	candidates := map[string]Candidate{}
	for _, candidate := range draft.Candidates {
		candidates[candidate.Path] = candidate
	}
	text := candidates["text.txt"]
	selected, err := service.SetContext(ctx, draft.ID, draft.Version, []ContextFile{
		{Path: text.Path, SHA256: text.SHA256, StartLine: 3, EndLine: 4},
		{Path: text.Path, SHA256: text.SHA256, StartLine: 1, EndLine: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.ContextSelection) != 1 || selected.ContextSelection[0].StartLine != 1 ||
		selected.ContextSelection[0].EndLine != 4 {
		t.Fatalf("context ranges were not merged: %+v", selected.ContextSelection)
	}
	large := candidates["large.txt"]
	if _, err := service.SetContext(ctx, draft.ID, selected.Version, []ContextFile{{
		Path: large.Path, SHA256: large.SHA256,
	}}); errorCode(err) != "recipe.draft_context_too_large" {
		t.Fatalf("large context error = %v", err)
	}
	binary := candidates["binary.bin"]
	if _, err := service.SetContext(ctx, draft.ID, selected.Version, []ContextFile{{
		Path: binary.Path, SHA256: binary.SHA256,
	}}); errorCode(err) != "recipe.draft_context_invalid" {
		t.Fatalf("binary context error = %v", err)
	}
}

func TestDirectoryBudgetCancelsAndGitEnvironmentIsSanitized(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "oversized"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, exceeded, stop := withDirectoryBudget(context.Background(), root, 4)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("directory budget did not cancel")
	}
	if !exceeded() {
		t.Fatal("directory budget cancellation did not record the exceeded limit")
	}

	t.Setenv("GIT_SSH_COMMAND", "malicious")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	for _, entry := range isolatedGitEnv() {
		if strings.HasPrefix(entry, "GIT_SSH_COMMAND=") || strings.HasPrefix(entry, "HTTPS_PROXY=") {
			t.Fatalf("unsafe environment survived sanitization: %s", entry)
		}
	}
}

func errorCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
