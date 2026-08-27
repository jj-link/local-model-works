package migrations

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRecipeRepositoryMigrationRetainsDuplicatePackages(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
CREATE TABLE recipes (
 digest TEXT PRIMARY KEY, name TEXT NOT NULL, version TEXT NOT NULL,
 display_name TEXT, description TEXT, license TEXT, source TEXT NOT NULL,
 trust_state TEXT NOT NULL, manifest TEXT NOT NULL, installed_at TEXT NOT NULL
);
CREATE TABLE recipe_update_checks (
 recipe_digest TEXT PRIMARY KEY, remote TEXT NOT NULL, tracking_ref TEXT NOT NULL,
 path TEXT NOT NULL, installed_revision TEXT NOT NULL, candidate_revision TEXT,
 state TEXT NOT NULL, checked_at TEXT NOT NULL, error TEXT
);`); err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	insertRecipe := func(digest, name, remote, installedAt string) {
		t.Helper()
		manifest := fmt.Sprintf(`{"metadata":{"name":%q,"source":{"url":%q,"path":".","revision":%q}}}`, name, remote, commit)
		if _, err := database.Exec(`INSERT INTO recipes
(digest,name,version,source,trust_state,manifest,installed_at)
VALUES (?,?, '1.0.0','{"type":"local"}','local',?,?)`, digest, name, manifest, installedAt); err != nil {
			t.Fatal(err)
		}
	}
	first := "sha256:" + strings.Repeat("1", 64)
	second := "sha256:" + strings.Repeat("2", 64)
	insertRecipe(first, "qwen-general", "HTTPS://GitHub.com/MiaAI-Lab/Qwen.git/", "2026-01-01T00:00:00Z")
	insertRecipe(second, "qwen-radixark", "https://github.com/MiaAI-Lab/Qwen", "2026-01-02T00:00:00Z")
	if _, err := database.Exec(`INSERT INTO recipe_update_checks
(recipe_digest,remote,tracking_ref,path,installed_revision,candidate_revision,state,checked_at)
VALUES (?, 'https://github.com/MiaAI-Lab/Qwen.git','main','.',?,?,'available','2026-01-03T00:00:00Z')`, first, commit, candidate); err != nil {
		t.Fatal(err)
	}

	migration, err := FS.ReadFile("009_recipe_repositories.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}

	var repositories int
	if err := database.QueryRow(`SELECT COUNT(*) FROM recipe_repositories`).Scan(&repositories); err != nil {
		t.Fatal(err)
	}
	if repositories != 1 {
		t.Fatalf("repositories = %d, want 1", repositories)
	}
	var versions, canonical int
	if err := database.QueryRow(`SELECT COUNT(*), SUM(canonical) FROM recipe_repository_versions`).Scan(&versions, &canonical); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || canonical != 1 {
		t.Fatalf("versions = %d, canonical = %d, want 2/1", versions, canonical)
	}
	var current, observed string
	if err := database.QueryRow(`SELECT current_digest, observed_head_commit FROM recipe_repositories`).Scan(&current, &observed); err != nil {
		t.Fatal(err)
	}
	if current != second || observed != candidate {
		t.Fatalf("current/observed = %q/%q, want %q/%q", current, observed, second, candidate)
	}
	var kept int
	if err := database.QueryRow(`SELECT COUNT(*) FROM recipes`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Fatalf("recipes retained = %d, want 2", kept)
	}
}
