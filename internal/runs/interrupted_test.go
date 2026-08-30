package runs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestMarkInterruptedReleasesOneShotRunLeases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	service := New(database, queries, events.NewEventBus(queries), root)
	runID, err := service.Create(ctx, "autoresearch", "autoresearch-factory", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.AcquireLeases(ctx, queries.WithTx(tx), "run", runID, []string{"autoresearch-project:project-1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != string(Interrupted) {
		t.Fatalf("state = %s, want interrupted", run.State)
	}
	if owners := service.ActiveOwners(ctx, "autoresearch-project:project-1"); len(owners) != 0 {
		t.Fatalf("interrupted run retained lease: %#v", owners)
	}
}
