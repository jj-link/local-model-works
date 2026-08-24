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
