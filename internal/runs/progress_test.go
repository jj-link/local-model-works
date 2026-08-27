package runs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestProgressAndOutputRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	service := New(database, queries, events.NewEventBus(queries), t.TempDir())
	runID, err := service.Create(ctx, "library", "recipe-update", map[string]any{"repository_id": "repo"}, "")
	if err != nil {
		t.Fatal(err)
	}
	progress := map[string]any{
		"phase": "pulling", "total_hardware": 2, "completed_hardware": 1,
		"hardware": []any{map[string]any{"node_id": "node-a", "current_step": 3, "total_steps": 5}},
	}
	output := map[string]any{"targets": []any{map[string]any{"node_id": "node-a", "status": "updated"}}}
	if err := service.SetProgress(ctx, runID, progress); err != nil {
		t.Fatal(err)
	}
	if err := service.SetOutput(ctx, runID, output); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	gotProgress, err := json.Marshal(got.Progress)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotProgress) != `{"completed_hardware":1,"hardware":[{"current_step":3,"node_id":"node-a","total_steps":5}],"phase":"pulling","total_hardware":2}` {
		t.Fatalf("progress = %s", gotProgress)
	}
	gotOutput, err := json.Marshal(got.Output)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOutput) != `{"targets":[{"node_id":"node-a","status":"updated"}]}` {
		t.Fatalf("output = %s", gotOutput)
	}
	row, err := queries.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Progress != `{"completed_hardware":1,"hardware":[{"current_step":3,"node_id":"node-a","total_steps":5}],"phase":"pulling","total_hardware":2}` {
		t.Fatalf("progress is not canonical: %s", row.Progress)
	}
}

func TestRecipeUpdateRunSurvivesInterruptedRecovery(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	service := New(database, queries, events.NewEventBus(queries), t.TempDir())
	runID, err := service.Create(ctx, "library", "recipe-update", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetState(ctx, runID, Running, "", ""); err != nil {
		t.Fatal(err)
	}
	count, err := service.MarkInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("interrupted count = %d, want 0", count)
	}
	got, err := service.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != string(Running) {
		t.Fatalf("state = %s, want running", got.State)
	}
}
