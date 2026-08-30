package recipe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestNewRecoversInterruptedPackageDeletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	packages := filepath.Join(root, "packages")
	if err := os.MkdirAll(packages, 0o700); err != nil {
		t.Fatal(err)
	}

	restoredHex := strings.Repeat("a", 64)
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{Digest: "sha256:" + restoredHex, Name: "restore", Version: "1", Source: "{}", Manifest: "{}"}); err != nil {
		t.Fatal(err)
	}
	restoredTombstone := filepath.Join(packages, restoredHex+".deleting-1")
	if err := os.MkdirAll(restoredTombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restoredTombstone, "index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	removedHex := strings.Repeat("b", 64)
	removedTombstone := filepath.Join(packages, removedHex+".deleting-1")
	if err := os.MkdirAll(removedTombstone, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := New(database, queries, events.NewEventBus(queries), validator, filepath.Join(root, "catalog"), packages); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(packages, restoredHex)); err != nil {
		t.Fatalf("installed package not restored: %v", err)
	}
	if _, err := os.Stat(restoredTombstone); !os.IsNotExist(err) {
		t.Fatalf("restored tombstone remains: %v", err)
	}
	if _, err := os.Stat(removedTombstone); !os.IsNotExist(err) {
		t.Fatalf("deleted recipe tombstone remains: %v", err)
	}
}
