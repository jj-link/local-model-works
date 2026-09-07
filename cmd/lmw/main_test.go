package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
)

func TestAdminCreatePasswordStdin(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer
	if err := runAdminCreate([]string{"--state", state, "--username", "operator", "--password-stdin"}, strings.NewReader("correct horse battery staple\n"), &out); err != nil {
		t.Fatalf("admin create: %v", err)
	}
	if !strings.Contains(out.String(), "operator operator created") {
		t.Fatalf("output = %q", out.String())
	}

	sqlDB, err := db.Open(context.Background(), filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	user, err := db.New(sqlDB).GetUser(context.Background(), "operator")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if !auth.VerifyPassword("correct horse battery staple", user.Argon2Hash) {
		t.Fatal("stored hash does not verify")
	}
	if err := runAdminCreate([]string{"--state", state, "--password-stdin"}, strings.NewReader("another password\n"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second create error = %v", err)
	}
}

func TestAdminBrowserLoginStoresOneUseTokenHash(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := runAdminCreate(
		[]string{"--state", state, "--username", "operator", "--password-stdin"},
		strings.NewReader("correct horse battery staple\n"), &bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAdminBrowserLogin([]string{"--state", state, "--username", "operator"}, &out); err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(out.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if len(issued.Token) != 64 {
		t.Fatalf("token length = %d", len(issued.Token))
	}
	expires, err := time.Parse(time.RFC3339Nano, issued.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(expires); remaining <= 0 || remaining > 61*time.Second {
		t.Fatalf("token lifetime = %s", remaining)
	}
	sqlDB, err := db.Open(context.Background(), filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var storedHash string
	if err := sqlDB.QueryRow(`SELECT token_hash FROM browser_login_tokens`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", auth.SHA256([]byte(issued.Token)))
	if storedHash == issued.Token || storedHash != wantHash {
		t.Fatalf("stored token = %q", storedHash)
	}
	var auditCount int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'auth.browser_login_token_created'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit events = %d", auditCount)
	}
}

func TestAdminCreatePasswordStdinRejectsMultipleLines(t *testing.T) {
	err := runAdminCreate([]string{"--state", t.TempDir(), "--password-stdin"}, strings.NewReader("first\nsecond\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exactly one line") {
		t.Fatalf("error = %v", err)
	}
}

func TestAdminAPITokenLifecycleNeverPrintsCredential(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(state, "factory-token")
	var out bytes.Buffer
	if err := runAdminAPITokenCreate([]string{
		"--state", state, "--name", "agon-factory",
		"--scope", auth.ScopeDeploymentsRead,
		"--scope", auth.ScopeBenchmarksRead,
		"--scope", auth.ScopeBenchmarksWrite,
		"--output-file", credential,
	}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), auth.APITokenPrefix) {
		t.Fatalf("create output exposed credential: %q", out.String())
	}
	first, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal(err)
	}
	firstToken := strings.TrimSpace(string(first))
	if !auth.ValidateAPIToken(firstToken) {
		t.Fatal("created credential has invalid format")
	}
	info, err := os.Stat(credential)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v, %v", info.Mode().Perm(), err)
	}
	database, err := db.Open(context.Background(), filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	queries := db.New(database)
	before, err := queries.GetAPITokenByName(context.Background(), "agon-factory")
	if err != nil {
		t.Fatal(err)
	}
	if before.TokenHash == firstToken || before.TokenHash != auth.APITokenHash(firstToken) {
		t.Fatal("database did not store only the credential hash")
	}
	database.Close()

	out.Reset()
	if err := runAdminAPITokenRotate([]string{
		"--state", state, "--name", "agon-factory", "--output-file", credential,
	}, &out); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal(err)
	}
	secondToken := strings.TrimSpace(string(second))
	if secondToken == firstToken || !auth.ValidateAPIToken(secondToken) || strings.Contains(out.String(), auth.APITokenPrefix) {
		t.Fatal("rotation did not replace the credential safely")
	}
	database, err = db.Open(context.Background(), filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := db.New(database).GetAPITokenByName(context.Background(), "agon-factory")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.TokenHash != auth.APITokenHash(secondToken) {
		t.Fatal("rotation changed identity or stored the wrong hash")
	}
	database.Close()

	out.Reset()
	if err := runAdminAPITokenRevoke([]string{"--state", state, "--name", "agon-factory"}, &out); err != nil {
		t.Fatal(err)
	}
	database, err = db.Open(context.Background(), filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := db.New(database).GetAPITokenByHash(context.Background(), auth.APITokenHash(secondToken)); err == nil {
		t.Fatal("revoked token still authenticates")
	}
}

func TestAdminAPITokenRejectsUnknownScopeBeforeWriting(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	credential := filepath.Join(state, "bad-token")
	err := runAdminAPITokenCreate([]string{
		"--state", state, "--name", "bad", "--scope", "settings:write", "--output-file", credential,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown API token scope") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Lstat(credential); !os.IsNotExist(err) {
		t.Fatal("unknown scope wrote a credential")
	}
}
