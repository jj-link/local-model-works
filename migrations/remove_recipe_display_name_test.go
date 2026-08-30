package migrations

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRemoveRecipeDisplayNameMigrationPreservesRecipe(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.Exec(`
CREATE TABLE recipes (
 digest TEXT PRIMARY KEY,
 name TEXT NOT NULL,
 version TEXT NOT NULL,
 display_name TEXT,
 manifest TEXT NOT NULL
);
INSERT INTO recipes (digest, name, version, display_name, manifest)
VALUES ('sha256:recipe', 'owner/repository', '1.0.0', 'Decorative name', '{}');
`); err != nil {
		t.Fatal(err)
	}

	migration, err := FS.ReadFile("014_remove_recipe_display_name.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}

	var name string
	if err := database.QueryRow(`SELECT name FROM recipes WHERE digest = 'sha256:recipe'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "owner/repository" {
		t.Fatalf("recipe name = %q", name)
	}

	rows, err := database.Query(`PRAGMA table_info(recipes)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if columnName == "display_name" {
			t.Fatal("display_name column remains after migration")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
