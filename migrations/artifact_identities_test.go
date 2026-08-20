package migrations

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestArtifactIdentityMigrationCanonicalizesRevision(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE artifacts (
 id TEXT PRIMARY KEY, kind TEXT NOT NULL, identity TEXT NOT NULL UNIQUE,
 revision TEXT, digest TEXT, validation_state TEXT, metadata TEXT, created_at TEXT
);
CREATE TABLE artifact_placements (
 id INTEGER PRIMARY KEY, artifact_id TEXT, node_id TEXT, path TEXT, state TEXT,
 verified_at TEXT, diagnostics TEXT, size_bytes INTEGER,
 UNIQUE (artifact_id, node_id, path)
);`); err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	if _, err := db.Exec(`INSERT INTO artifacts
(id,kind,identity,revision,validation_state,metadata,created_at)
VALUES ('old','model','huggingface://Acme/Model',?,'valid','{}','now')`, revision); err != nil {
		t.Fatal(err)
	}
	migration, err := FS.ReadFile("005_artifact_identities.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var identity string
	if err := db.QueryRow(`SELECT identity FROM artifacts WHERE id='old'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	want := "hf://Acme/Model@" + revision
	if identity != want {
		t.Fatalf("identity = %q, want %q", identity, want)
	}
}
