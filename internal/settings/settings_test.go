package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
)

func TestSetCreatesUpdatesAndRejectsStaleETag(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	registry := New(db.New(sqlDB))
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["enabled"],"properties":{"enabled":{"type":"boolean"}}}`)
	if err := registry.Register("test", schema); err != nil {
		t.Fatal(err)
	}
	first, err := registry.Set(ctx, "test", map[string]any{"enabled": true}, "0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Set(ctx, "test", map[string]any{"enabled": false}, first)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("settings version did not advance")
	}
	if _, err := registry.Set(ctx, "test", map[string]any{"enabled": true}, first); !errors.Is(err, ErrStale) {
		t.Fatalf("stale update error = %v", err)
	}
	value, version, err := registry.Get(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if value["enabled"] != false || version != second {
		t.Fatalf("settings=%v version=%s", value, version)
	}
}
