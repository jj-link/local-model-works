package migrations

import (
	"bytes"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAutoResearchMigrationPreservesSecretsAndAddsPurposes(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
CREATE TABLE secrets (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 purpose TEXT NOT NULL CHECK (purpose IN ('huggingface','github','registry')),
 nonce BLOB NOT NULL,
 ciphertext BLOB NOT NULL,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL
);
INSERT INTO secrets VALUES ('secret-1','existing','github',x'0102',x'0304','created','updated');`); err != nil {
		t.Fatal(err)
	}
	migration, err := FS.ReadFile("007_autoresearch.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var id, name, purpose, created, updated string
	var nonce, ciphertext []byte
	if err := database.QueryRow(`SELECT id,name,purpose,nonce,ciphertext,created_at,updated_at FROM secrets WHERE id='secret-1'`).Scan(
		&id, &name, &purpose, &nonce, &ciphertext, &created, &updated,
	); err != nil {
		t.Fatal(err)
	}
	if id != "secret-1" || name != "existing" || purpose != "github" || created != "created" || updated != "updated" ||
		!bytes.Equal(nonce, []byte{1, 2}) || !bytes.Equal(ciphertext, []byte{3, 4}) {
		t.Fatalf("secret changed: %q %q %q %x %x %q %q", id, name, purpose, nonce, ciphertext, created, updated)
	}
	for _, newPurpose := range []string{"model_provider", "ssh"} {
		if _, err := database.Exec(`INSERT INTO secrets(id,name,purpose,nonce,ciphertext) VALUES(?,?,?,?,?)`, newPurpose, newPurpose, newPurpose, []byte{1}, []byte{2}); err != nil {
			t.Fatalf("insert purpose %s: %v", newPurpose, err)
		}
	}
}

func TestAutoResearchHardeningMigrationNormalizesIdeasAndRunner(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
CREATE TABLE secrets (
 id TEXT PRIMARY KEY,
 name TEXT NOT NULL UNIQUE,
 purpose TEXT NOT NULL CHECK (purpose IN ('huggingface','github','registry')),
 nonce BLOB NOT NULL,
 ciphertext BLOB NOT NULL,
 created_at TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL DEFAULT ''
);`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"007_autoresearch.sql", "008_autoresearch_hardening.sql"} {
		migration, readErr := FS.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if name == "008_autoresearch_hardening.sql" {
			if _, err := database.Exec(`
INSERT INTO autoresearch_projects (id, name, status, runner_node_id, idea_prompt)
VALUES ('project-1', 'Project', 'idea_intake', NULL, 'Question');
INSERT INTO autoresearch_ideas
    (id, project_id, ordinal, source, title, body, selected, updated_at)
VALUES
    ('human', 'project-1', 1, 'human', 'Human', 'Human body', 1, '2026-01-01T00:00:00Z'),
    ('generated-selected', 'project-1', 2, 'generated', 'Selected', 'Selected body', 1, '2026-01-02T00:00:00Z'),
    ('generated-other', 'project-1', 10, 'generated', 'Other', 'Other body', 0, '2026-01-03T00:00:00Z');`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.Exec(string(migration)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	var humanOrdinal, selectedCount int
	if err := database.QueryRow(`SELECT ordinal FROM autoresearch_ideas WHERE id = 'human'`).Scan(&humanOrdinal); err != nil {
		t.Fatal(err)
	}
	if humanOrdinal != 0 {
		t.Fatalf("human ordinal = %d, want 0", humanOrdinal)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM autoresearch_ideas WHERE project_id = 'project-1' AND selected = 1`).Scan(&selectedCount); err != nil {
		t.Fatal(err)
	}
	if selectedCount != 1 {
		t.Fatalf("selected count = %d, want 1", selectedCount)
	}
	var selectedID string
	if err := database.QueryRow(`SELECT id FROM autoresearch_ideas WHERE selected = 1`).Scan(&selectedID); err != nil {
		t.Fatal(err)
	}
	if selectedID != "generated-selected" {
		t.Fatalf("selected idea = %q, want generated-selected", selectedID)
	}
	if _, err := database.Exec(`
INSERT INTO autoresearch_ideas (id, project_id, ordinal, source, title, body)
VALUES ('generated-eleven', 'project-1', 11, 'generated', 'Eleven', 'Body')`); err != nil {
		t.Fatalf("insert ordinal above ten: %v", err)
	}
	if _, err := database.Exec(`
INSERT INTO autoresearch_ideas (id, project_id, ordinal, source, title, body)
VALUES ('generated-negative', 'project-1', -1, 'generated', 'Negative', 'Body')`); err == nil {
		t.Fatal("negative ordinal unexpectedly accepted")
	}
	rows, err := database.Query(`PRAGMA table_info(autoresearch_projects)`)
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
		if name == "runner_node_id" {
			t.Fatal("runner_node_id column still exists")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
