package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

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

func TestAdminCreatePasswordStdinRejectsMultipleLines(t *testing.T) {
	err := runAdminCreate([]string{"--state", t.TempDir(), "--password-stdin"}, strings.NewReader("first\nsecond\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exactly one line") {
		t.Fatalf("error = %v", err)
	}
}
