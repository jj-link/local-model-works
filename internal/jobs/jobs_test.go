package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/runs"
)

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
