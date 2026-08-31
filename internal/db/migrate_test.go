package db

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/jj-link/local-model-works/migrations"
	_ "modernc.org/sqlite"
)

func TestDeploymentParametersMigrationPreservesReferencedRows(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite", "file:deployment-parameters-migration?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	legacy := fstest.MapFS{}
	for version := 1; version <= 14; version++ {
		name := fmt.Sprintf("%03d_", version)
		entries, readErr := migrations.FS.ReadDir(".")
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if len(entry.Name()) >= len(name) && entry.Name()[:len(name)] == name {
				body, bodyErr := migrations.FS.ReadFile(entry.Name())
				if bodyErr != nil {
					t.Fatal(bodyErr)
				}
				legacy[entry.Name()] = &fstest.MapFile{Data: body}
				break
			}
		}
	}
	if err := Migrate(ctx, database, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `
INSERT INTO recipes (digest, name, version, source, manifest)
VALUES ('sha256:recipe', 'owner/model', '1.0.0', '{}', '{}');
INSERT INTO deployments (id, recipe_digest, profile, placement, run_id)
VALUES ('deployment-1', 'sha256:recipe', 'quality', '{"ranks":{"node-1":0}}', 'run-1');
INSERT INTO runs (id, module, kind, deployment_id)
VALUES ('run-1', 'serving', 'deployment-create', 'deployment-1');
`); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, database, migrations.FS); err != nil {
		t.Fatal(err)
	}

	var parameters string
	if err := database.QueryRowContext(ctx,
		`SELECT parameters FROM deployments WHERE id = 'deployment-1'`,
	).Scan(&parameters); err != nil {
		t.Fatal(err)
	}
	if parameters != "{}" {
		t.Fatalf("parameters = %q, want {}", parameters)
	}
	var deploymentID sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT deployment_id FROM runs WHERE id = 'run-1'`,
	).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	if !deploymentID.Valid || deploymentID.String != "deployment-1" {
		t.Fatalf("run deployment_id = %+v, want deployment-1", deploymentID)
	}
}
