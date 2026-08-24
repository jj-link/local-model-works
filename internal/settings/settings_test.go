package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
)

func TestSetInsertsUpdatesAndRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registry := New(db.New(database))
	if err := registry.Register("fixture", json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"}}}`)); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Set(ctx, "fixture", map[string]any{"enabled": true}, "0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Set(ctx, "fixture", map[string]any{"enabled": false}, first)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second == "0" {
		t.Fatalf("versions did not advance: %q -> %q", first, second)
	}
	if _, err := registry.Set(ctx, "fixture", map[string]any{"enabled": true}, first); !errors.Is(err, ErrStale) {
		t.Fatalf("stale update error = %v", err)
	}
	stored, version, err := registry.Get(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if version != second || stored["enabled"] != false {
		t.Fatalf("stored settings = %v at %s", stored, version)
	}
}
