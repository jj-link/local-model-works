package recipebuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
)

func savedChangeFixture(t *testing.T) (*Service, recipe.Recipe) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	q := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "packages")
	t.Cleanup(func() { _ = recipe.RemovePackage(packageRoot) })
	recipes, err := recipe.New(database, q, events.NewEventBus(q), validator, filepath.Join(root, "catalog"), packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	imageDigest := "sha256:" + strings.Repeat("f", 64)
	manifest := []byte(fmt.Sprintf(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"change-fixture","version":"1.0.0","description":"Saved baseline fixture","license":"MIT","source":{"url":"https://github.com/example/unavailable-history","revision":"%s","path":"."}},"compatibility":{"nodeCount":1},"artifacts":[],"workloads":[{"image":{"reference":"example.invalid/image@%s","digest":"%s"},"command":["/bin/sh","/lmw/assets/serve.sh"],"args":[],"resources":{"pids":64}}],"assets":["serve.sh"]}`, strings.Repeat("a", 40), imageDigest, imageDigest))
	packed, err := recipe.PackManifest(manifest, map[string][]byte{"serve.sh": []byte("#!/bin/sh\necho saved\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(root, "input")
	if err := recipe.WriteLayout(layout, packed); err != nil {
		t.Fatal(err)
	}
	saved, err := recipes.Import(ctx, recipe.RecipeSource{Type: "local", Path: layout})
	if err != nil {
		t.Fatal(err)
	}
	service := New(q, root, validator, recipes)
	service.SetDB(database)
	return service, saved
}

func TestAddComparisonWithoutSavedBaseline(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	for _, suggested := range []bool{false, true} {
		t.Run(fmt.Sprintf("suggestion=%t", suggested), func(t *testing.T) {
			name := "edited"
			var proposalJSON any
			if suggested {
				name = "suggested"
				proposal, err := json.Marshal(Proposal{Manifest: json.RawMessage(`{"metadata":{"name":"suggested"}}`)})
				if err != nil {
					t.Fatal(err)
				}
				proposalJSON = string(proposal)
			}
			if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET change_context=?, manifest=?, proposal=? WHERE id=?`,
				`{"kind":"add","base_source_status":"unavailable"}`, `{"metadata":{"name":"edited"}}`, proposalJSON, draft.ID); err != nil {
				t.Fatal(err)
			}
			comparison, err := service.Compare(ctx, draft.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(comparison.ConfigurationChanges) != 1 {
				t.Fatalf("expected new configuration as added: %+v", comparison.ConfigurationChanges)
			}
			change := comparison.ConfigurationChanges[0]
			if change.Path != "/metadata" || len(change.Before) != 0 || !jsonEqual(change.After, json.RawMessage(fmt.Sprintf(`{"name":%q}`, name))) {
				t.Fatalf("incorrect added configuration: %+v", change)
			}
			if comparison.BaseRecipeDigest != "" || comparison.BaseCommit != "" || len(comparison.Files) != 0 {
				t.Fatalf("invented saved source: %+v", comparison)
			}
		})
	}
}

func TestRepairWithoutPriorDraftRetainsVerifiedPackageWithoutFetching(t *testing.T) {
	service, saved := savedChangeFixture(t)
	ctx := context.Background()
	draft, err := service.AllocateChange(ctx, saved.Digest, ChangeRequest{Kind: "repair", ExpectedCurrentDigest: saved.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if draft.ChangeContext.BaseSourceStatus != "unavailable" || draft.ParentDraftID != "" {
		t.Fatalf("invented retained source: %+v", draft.ChangeContext)
	}
	before, err := service.recipes.Get(ctx, saved.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(before.Manifest, draft.Manifest) {
		t.Fatal("repair reconstructed instead of preserving saved configuration")
	}
	if len(draft.SelectedAssets) != 1 {
		t.Fatalf("saved helpers lost: %+v", draft.SelectedAssets)
	}
	asset := draft.SelectedAssets[0]
	_, body, err := service.ReadOwnedFile(ctx, draft.ID, asset.Path, asset.SHA256, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "#!/bin/sh\necho saved\n" {
		t.Fatalf("saved helper changed: %q", body)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, draft.Version, PhaseInspect)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.PrepareChange(ctx, draft.ID, reserved.Operation.ID, "", nil)
	if err != nil {
		t.Fatalf("repair unexpectedly required remote source: %v", err)
	}
	if ready.Operation != nil || !jsonEqual(ready.Manifest, before.Manifest) {
		t.Fatal("repair did not preserve editable baseline")
	}
	repository, err := service.q.GetRecipeRepository(ctx, draft.ChangeContext.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if value(repository.CurrentDigest) != saved.Digest {
		t.Fatal("preparing repair changed saved current version")
	}
}

func TestRepairRootSourceRemainsValidAfterEditing(t *testing.T) {
	service, saved := savedChangeFixture(t)
	ctx := context.Background()
	draft, err := service.AllocateChange(ctx, saved.Digest, ChangeRequest{Kind: "repair", ExpectedCurrentDigest: saved.Digest})
	if err != nil {
		t.Fatal(err)
	}
	// URL-only additions represent the repository root with an omitted path.
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET source=json_remove(source,'$.path') WHERE id=?`, draft.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{
		Manifest: draft.Manifest, SelectedAssets: draft.SelectedAssets,
	})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := service.validator.Validate(updated.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			t.Fatalf("editing invalidated the saved root-source recipe: %+v", diagnostic)
		}
	}
}

func TestChangeRejectsStaleBaselineBeforeAllocating(t *testing.T) {
	service, saved := savedChangeFixture(t)
	_, err := service.AllocateChange(context.Background(), saved.Digest, ChangeRequest{Kind: "repair", ExpectedCurrentDigest: "sha256:" + strings.Repeat("b", 64)})
	if errorCode(err) != "recipe.draft_stale_version" {
		t.Fatalf("stale baseline: %v", err)
	}
	drafts, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatal("stale request allocated work")
	}
}

func TestTwoCommitComparisonPreservesRemovedAndCollidingEvidence(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	root := filepath.Join(filepath.Dir(service.root), "git-fixture")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = root
		body, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, body)
		}
		return strings.TrimSpace(string(body))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("old source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "removed"), []byte("removed source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "before")
	beforeCommit := git("rev-parse", "HEAD")
	before, _, _, err := service.ingestAndCopy(root, filepath.Join(service.root, draft.ID, "source"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("new source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "removed")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "after")
	afterCommit := git("rev-parse", "HEAD")
	after, _, _, err := service.ingestAndCopy(root, filepath.Join(service.root, draft.ID, "source"))
	if err != nil {
		t.Fatal(err)
	}
	change := ChangeContext{Kind: "update", BaseCommit: beforeCommit, BaseSourceStatus: "available", BaseCandidates: before, BaseManifest: json.RawMessage(`{"ordered":[1,2],"a/b":{"~key":1},"removed":true}`)}
	contextJSON, _ := json.Marshal(change)
	candidateJSON, _ := json.Marshal(after)
	proposal := Proposal{ID: "compiler", BaseVersion: draft.Version, Manifest: json.RawMessage(`{"ordered":[2,1],"a/b":{"~key":2},"added":null}`)}
	proposalJSON, _ := json.Marshal(proposal)
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET change_context=?, resolved_commit=?, resolved_tree=?, candidates=?, proposal=? WHERE id=?`, string(contextJSON), afterCommit, git("rev-parse", "HEAD^{tree}"), string(candidateJSON), string(proposalJSON), draft.ID); err != nil {
		t.Fatal(err)
	}
	comparison, err := service.Compare(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparison.Files) != 3 || comparison.Files[0].Path != "README" || comparison.Files[0].Change != "modified" || !comparison.Files[1].Binary || comparison.Files[2].Change != "removed" {
		t.Fatalf("incorrect two-revision diff: %+v", comparison.Files)
	}
	pointers := []string{}
	for _, change := range comparison.ConfigurationChanges {
		pointers = append(pointers, change.Path)
	}
	if strings.Join(pointers, ",") != "/a~1b/~0key,/added,/ordered,/removed" {
		t.Fatalf("unsafe config diff: %v", pointers)
	}
	old := before[0]
	_, body, err := service.ReadOwnedFile(ctx, draft.ID, old.Path, old.SHA256, beforeCommit)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old source\n" {
		t.Fatalf("old source collision lost: %q", body)
	}
}

func TestUnavailableRetainedSourceDoesNotLoseSavedHelpers(t *testing.T) {
	service, saved := savedChangeFixture(t)
	ctx := context.Background()
	root := filepath.Join(filepath.Dir(service.root), "historical-source")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("original evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	detail, err := service.recipes.Get(ctx, saved.Digest)
	if err != nil {
		t.Fatal(err)
	}
	original, err := service.CreateFromDir(ctx, GitSource{Remote: "https://github.com/example/unavailable-history"}, strings.Repeat("a", 40), strings.Repeat("b", 40), detail.Manifest, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET package_digest=?, state='installed' WHERE id=?`, saved.Digest, original.ID); err != nil {
		t.Fatal(err)
	}
	candidate := original.Candidates[0]
	blob := filepath.Join(service.root, original.ID, "source", "sha256-"+candidate.SHA256)
	if err := os.Chmod(blob, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("corrupt! evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	draft, err := service.AllocateChange(ctx, saved.Digest, ChangeRequest{Kind: "repair", ExpectedCurrentDigest: saved.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if draft.ChangeContext.BaseSourceStatus != "unavailable" || len(draft.ChangeContext.BaseCandidates) != 0 {
		t.Fatal("corrupt historical source was accepted")
	}
	assets, err := service.verifySelectedAssets(draft)
	if err != nil {
		t.Fatal(err)
	}
	if string(assets["serve.sh"]) != "#!/bin/sh\necho saved\n" {
		t.Fatal("historical source failure lost exact saved helper")
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_repositories SET current_digest=NULL WHERE id=?`, draft.ChangeContext.RepositoryID); err != nil {
		t.Fatal(err)
	}
	if err := service.CheckSaveBaseline(ctx, draft.ID); errorCode(err) != "recipe.draft_stale_version" {
		t.Fatalf("late current change escaped save preflight: %v", err)
	}
}
