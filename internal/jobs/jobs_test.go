package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/runs"
)

func TestMergedSecretScopes(t *testing.T) {
	input := map[string]any{"provider_secret": "provider-main", "ssh_secret": "spark-key"}
	spec := Spec{
		SecretScopes: []string{"static", "provider-main"},
		SecretScopesFor: func(got map[string]any) []string {
			if got["provider_secret"] != input["provider_secret"] {
				t.Fatalf("selector received %#v", got)
			}
			return []string{"provider-main", "", "spark-key", "spark-key"}
		},
	}
	got := mergedSecretScopes(spec, input)
	want := []string{"static", "provider-main", "spark-key"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %v, want %v", got, want)
		}
	}
}

func TestJobResourceContracts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	runsSvc := runs.New(database, queries, events.NewEventBus(queries), root)
	registry := New(runsSvc, root, ctx, database, queries)
	box, err := auth.NewSecretBox(filepath.Join(root, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	registry.SetSecretBox(box)
	nonce, ciphertext, err := box.Seal("secret-1", "token-value", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateSecret(ctx, db.CreateSecretParams{ID: "secret-1", Name: "github-main", Purpose: "github", Nonce: nonce, Ciphertext: ciphertext}); err != nil {
		t.Fatal(err)
	}

	seen := make(chan struct{}, 1)
	if err := registry.Register("test", Spec{
		Kind: "resource-job", Title: "Resource job",
		InputSchema:           json.RawMessage(`{"type":"object","required":["languages"],"properties":{"languages":{"type":"array","items":{"type":"string"}}}}`),
		OutputSchema:          json.RawMessage(`{"type":"object","required":["results"],"properties":{"results":{"type":"array","items":{"type":"object","required":["language"],"properties":{"language":{"type":"string"}}}}}}`),
		SecretScopes:          []string{"github-main"},
		PlacementRequirements: []string{"accelerator:nvidia"},
		LeaseResources:        func(map[string]any) []string { return []string{"node:test"} },
		ArtifactKinds:         []string{"file"},
		Executor: func(_ context.Context, job *Context) (map[string]any, error) {
			languages, ok := job.Input["languages"].([]any)
			if !ok || len(languages) != 1 || languages[0] != "go" {
				t.Errorf("normalized input = %#v", job.Input)
			}
			if job.Secrets["github-main"] != "token-value" {
				t.Errorf("secret not scoped to executor")
			}
			if len(job.Placements) != 1 || job.Placements[0] != "accelerator:nvidia" {
				t.Errorf("placements = %v", job.Placements)
			}
			path := filepath.Join(job.Workspace, "result.txt")
			if err := os.WriteFile(path, []byte("result"), 0o600); err != nil {
				return nil, err
			}
			artifact, err := job.PublishArtifact("file", path)
			if err != nil {
				return nil, err
			}
			if artifact.Identity == "" || artifact.Size != 6 {
				t.Errorf("artifact = %+v", artifact)
			}
			seen <- struct{}{}
			return map[string]any{"results": []map[string]any{{"language": "go"}}}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	runID, err := registry.Submit(ctx, "resource-job", map[string]any{"languages": []string{"go"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	case <-time.After(15 * time.Second):
		t.Fatal("executor did not run")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := runsSvc.Get(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == string(runs.Succeeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run state = %s", run.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var leaseCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE owner_kind = ? AND owner_id = ? AND state = 'active'", "run", runID).Scan(&leaseCount); err != nil {
		t.Fatal(err)
	}
	if leaseCount != 0 {
		t.Fatalf("terminal job retained %d leases", leaseCount)
	}
}

func TestProjectLeaseConflictAndChainedTransfer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	runsSvc := runs.New(database, queries, events.NewEventBus(queries), root)
	registry := New(runsSvc, root, ctx, database, queries)

	started := make(chan string, 4)
	parentRelease := make(chan struct{})
	childRelease := make(chan struct{})
	otherRelease := make(chan struct{})
	if err := registry.Register("test", Spec{
		Kind: "project-job",
		LeaseResources: func(input map[string]any) []string {
			return []string{"autoresearch-project:" + input["project_id"].(string)}
		},
		Executor: func(_ context.Context, job *Context) (map[string]any, error) {
			role, _ := job.Input["role"].(string)
			started <- role
			switch role {
			case "parent":
				<-parentRelease
			case "child":
				<-childRelease
			case "other":
				<-otherRelease
			}
			return map[string]any{"role": role}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	parentID, err := registry.Submit(ctx, "project-job", map[string]any{"project_id": "project-1", "role": "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if role := <-started; role != "parent" {
		t.Fatalf("first executor = %q", role)
	}
	if _, err := registry.Submit(ctx, "project-job", map[string]any{"project_id": "project-1", "role": "conflict"}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("same-project submission error = %v, want ErrLeaseConflict", err)
	}
	if _, err := registry.Submit(ctx, "project-job", map[string]any{"project_id": "project-2", "role": "other"}); err != nil {
		t.Fatalf("different-project submission: %v", err)
	}
	if role := <-started; role != "other" {
		t.Fatalf("concurrent executor = %q", role)
	}

	childID, err := registry.SubmitChained(ctx, parentID, "project-job", map[string]any{"project_id": "project-1", "role": "child"})
	if err != nil {
		t.Fatal(err)
	}
	if role := <-started; role != "child" {
		t.Fatalf("chained executor = %q", role)
	}
	owners := runsSvc.ActiveOwners(ctx, "autoresearch-project:project-1")
	if len(owners) != 1 || owners[0].OwnerKind != "run" || owners[0].OwnerID != childID {
		t.Fatalf("lease owners after transfer = %#v, want run/%s", owners, childID)
	}

	close(parentRelease)
	close(childRelease)
	close(otherRelease)
	deadline := time.Now().Add(5 * time.Second)
	for {
		child, getErr := runsSvc.Get(ctx, childID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if runs.State(child.State).Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child remained %s", child.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if owners := runsSvc.ActiveOwners(ctx, "autoresearch-project:project-1"); len(owners) != 0 {
		t.Fatalf("terminal child retained lease: %#v", owners)
	}
}
