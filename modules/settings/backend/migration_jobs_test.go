package backend

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/migrate"
	"github.com/jj-link/local-model-works/internal/moduleapi"
)

func TestMigrationImportResumesCompletedCheckpoint(t *testing.T) {
	state := t.TempDir()
	module := &Module{env: &moduleapi.Env{RunRoot: filepath.Join(state, "runs")}}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := filepath.Join(state, "migrations", digest[7:])
	if err := writeCheckpoint(dir, migrationCheckpoint{Phase: "complete", Digest: digest, Report: &migrate.ImportReport{PlanDigest: digest, SourceUntouched: true, RecipesImported: 3, RunsImported: 4}}); err != nil {
		t.Fatal(err)
	}
	output, err := module.importMigration(context.Background(), &jobs.Context{Input: map[string]any{
		"plan_digest": digest, "legacy_root": "/legacy", "legacy_state": "/legacy-state", "confirm": true,
	}, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	if output["plan_digest"] != digest || output["recipes_imported"] != 3 || output["runs_imported"] != 4 || output["source_untouched"] != true {
		t.Fatalf("output = %#v", output)
	}
}

func TestMigrationImportRequiresExplicitConfirmation(t *testing.T) {
	module := &Module{env: &moduleapi.Env{RunRoot: filepath.Join(t.TempDir(), "runs")}}
	_, err := module.importMigration(context.Background(), &jobs.Context{Input: map[string]any{
		"plan_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"legacy_root": "/legacy", "legacy_state": "/legacy-state", "confirm": false,
	}})
	if err == nil {
		t.Fatal("unconfirmed import was accepted")
	}
}
