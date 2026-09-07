package migrations

import (
	"bytes"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRecipeAssistantSecretMigrationPreservesCiphertextAndIdentity(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	base, err := FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(base)); err != nil {
		t.Fatal(err)
	}
	nonce := []byte{1, 2, 3}
	ciphertext := []byte{4, 5, 6, 7}
	created, updated := "2024-01-01T00:00:00Z", "2024-02-01T00:00:00Z"
	if _, err := database.Exec(`INSERT INTO secrets(id,name,purpose,nonce,ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		"secret-1", "existing", "registry", nonce, ciphertext, created, updated); err != nil {
		t.Fatal(err)
	}
	migration, err := FS.ReadFile("018_recipe_assistant_secrets.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	var gotID, gotName, gotPurpose, gotCreated, gotUpdated string
	var gotNonce, gotCiphertext []byte
	if err := database.QueryRow(`SELECT id,name,purpose,nonce,ciphertext,created_at,updated_at FROM secrets WHERE id='secret-1'`).Scan(
		&gotID, &gotName, &gotPurpose, &gotNonce, &gotCiphertext, &gotCreated, &gotUpdated); err != nil {
		t.Fatal(err)
	}
	if gotID != "secret-1" || gotName != "existing" || gotPurpose != "registry" || gotCreated != created || gotUpdated != updated ||
		!bytes.Equal(gotNonce, nonce) || !bytes.Equal(gotCiphertext, ciphertext) {
		t.Fatal("existing encrypted secret changed during migration")
	}
	if _, err := database.Exec(`INSERT INTO secrets(id,name,purpose,nonce,ciphertext) VALUES('secret-2','assistant','recipe-assistant',x'01',x'02')`); err != nil {
		t.Fatalf("recipe-assistant purpose rejected: %v", err)
	}
}
