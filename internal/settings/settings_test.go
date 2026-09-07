package settings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
)

func TestSetCreatesAndAtomicallyUpdatesModuleSettings(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	registry := New(db.New(database))
	if err := registry.Register("library", nil); err != nil {
		t.Fatal(err)
	}

	version, err := registry.Set(ctx, "library", map[string]any{"value": "first"}, "0")
	if err != nil {
		t.Fatalf("first set: %v", err)
	}
	if version == "" || version == "0" {
		t.Fatalf("first version = %q", version)
	}

	next, err := registry.Set(ctx, "library", map[string]any{"value": "second"}, version)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := registry.Set(ctx, "library", map[string]any{"value": "stale"}, version); !errors.Is(err, ErrStale) {
		t.Fatalf("stale update error = %v", err)
	}

	value, gotVersion, err := registry.Get(ctx, "library")
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != next || value["value"] != "second" {
		t.Fatalf("stored settings = %#v, version = %q", value, gotVersion)
	}
}
