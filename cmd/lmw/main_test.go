package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
