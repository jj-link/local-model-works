package migrations

import (
	"database/sql"
	"strings"
	"testing"
)

func TestProcedureMigrationPreservesCatalogAndSeparatesLaunchProcedures(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	exec := func(query string) {
		t.Helper()
		if _, err := database.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	exec("PRAGMA foreign_keys=ON")
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "023_recipe_procedures.sql" {
			continue
		}
		migration, err := FS.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		exec(string(migration))
	}
	exec(`INSERT INTO recipes(digest,name,version,manifest) VALUES
		('saved','upstream','1.0.0','{}'),('reviewed','upstream','1.0.1','{}'),('separate','upstream-tp2','1.0.0','{}');
		INSERT INTO recipe_repositories(id,source_url,source_path,current_digest) VALUES
		('original','https://github.com/org/upstream','.','reviewed');
		INSERT INTO recipe_repository_versions(repository_id,recipe_digest,commit_sha,canonical,installed_at) VALUES
		('original','saved','commit',1,'2026-01-01'),('original','reviewed','commit',0,'2026-01-02')`)
	migration, err := FS.ReadFile("023_recipe_procedures.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	var current, canonical, reviewed string
	if err := database.QueryRow(`SELECT r.current_digest,
		(SELECT recipe_digest FROM recipe_repository_versions WHERE repository_id=r.id AND canonical=1),
		(SELECT recipe_digest FROM recipe_repository_versions WHERE repository_id=r.id AND canonical=0)
		FROM recipe_repositories r WHERE r.id='original'`).Scan(&current, &canonical, &reviewed); err != nil {
		t.Fatal(err)
	}
	if current != "reviewed" || canonical != "saved" || reviewed != "reviewed" {
		t.Fatalf("migration changed saved catalog history: current=%q canonical=%q reviewed=%q", current, canonical, reviewed)
	}
	exec(`INSERT INTO recipe_repositories(id,source_url,source_path,procedure,current_digest) VALUES
		('tp2','https://github.com/org/upstream','.','two-spark','separate');
		INSERT INTO recipe_repository_versions(repository_id,recipe_digest,commit_sha,canonical,installed_at) VALUES
		('tp2','separate','commit',1,'2026-01-03')`)
	if _, err := database.Exec(`INSERT INTO recipe_repositories(id,source_url,source_path,procedure) VALUES
		('duplicate','https://github.com/org/upstream','.','two-spark')`); err == nil {
		t.Fatal("duplicate launch procedure identity was accepted")
	}
	if _, err := database.Exec(`DELETE FROM recipes WHERE digest='reviewed'`); err == nil {
		t.Fatal("migration lost protection of the current saved recipe")
	}
	exec(`DELETE FROM recipe_repositories WHERE id='original'`)
	if err := database.QueryRow(`SELECT recipe_digest FROM recipe_repository_versions WHERE repository_id='tp2'`).Scan(&current); err != nil || current != "separate" {
		t.Fatalf("removing one catalog entry affected another procedure: %q, %v", current, err)
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM recipe_repository_versions WHERE repository_id='original'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("removed catalog entry left orphaned versions: %d, %v", count, err)
	}
}
