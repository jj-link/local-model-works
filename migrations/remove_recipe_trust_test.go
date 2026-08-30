package migrations

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRemoveRecipeTrustMigrationPreservesReferences(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE recipes (
 digest TEXT PRIMARY KEY, name TEXT NOT NULL, version TEXT NOT NULL,
 trust_state TEXT NOT NULL DEFAULT 'untrusted', manifest TEXT NOT NULL
);
CREATE TABLE deployments (
 id TEXT PRIMARY KEY,
 recipe_digest TEXT NOT NULL REFERENCES recipes(digest) ON DELETE RESTRICT
);
INSERT INTO recipes (digest, name, version, trust_state, manifest)
VALUES ('sha256:recipe', 'recipe', '1.0.0', 'local', '{}');
INSERT INTO deployments (id, recipe_digest) VALUES ('deployment', 'sha256:recipe');
`); err != nil {
		t.Fatal(err)
	}
	migration, err := FS.ReadFile("013_remove_recipe_trust.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var recipeDigest string
	if err := database.QueryRow(`SELECT recipe_digest FROM deployments WHERE id = 'deployment'`).Scan(&recipeDigest); err != nil {
		t.Fatal(err)
	}
	if recipeDigest != "sha256:recipe" {
		t.Fatalf("deployment recipe digest = %q", recipeDigest)
	}
	rows, err := database.Query(`PRAGMA table_info(recipes)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "trust_state" {
			t.Fatal("trust_state column remains after migration")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
