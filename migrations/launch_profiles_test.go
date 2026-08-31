package migrations

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLaunchProfilesMigrationCreatesUniqueDigestName(t *testing.T) {
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
 manifest TEXT NOT NULL
);
INSERT INTO recipes (digest, name, version, manifest)
VALUES ('sha256:recipe', 'owner/repository', '1.0.0', '{}');
`); err != nil {
		t.Fatal(err)
	}

	migration, err := FS.ReadFile("015_launch_profiles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}

	insert := func(id, name string) error {
		_, err := database.Exec(
			`INSERT INTO launch_profiles (id, name, recipe_digest, variants, parameters)
			 VALUES (?, ?, 'sha256:recipe', '{}', '{}')`, id, name)
		return err
	}
	if err := insert("lp-1", "big"); err != nil {
		t.Fatal(err)
	}
	if err := insert("lp-2", "big"); err == nil {
		t.Fatal("duplicate (recipe_digest, name) must violate the unique constraint")
	}
	if err := insert("lp-3", "small"); err != nil {
		t.Fatal(err, "same name under a different recipe stays legal once another recipe exists")
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM launch_profiles WHERE recipe_digest = 'sha256:recipe'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("launch profile rows = %d, want 2 (duplicate rejected per digest)", count)
	}

	rows, err := database.Query(`PRAGMA table_info(launch_profiles)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	for _, want := range []string{"id", "name", "recipe_digest", "variants", "parameters", "created_at", "updated_at"} {
		if !columns[want] {
			t.Fatalf("launch_profiles missing column %s", want)
		}
	}
}
