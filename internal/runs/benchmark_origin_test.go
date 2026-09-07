package runs

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestListBenchmarkOriginPaginatesEveryRunWithEqualTimestamps(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	createdAt := "2025-01-01T00:00:00.000Z"
	for index := range 205 {
		id := fmt.Sprintf("01900000-0000-7000-8000-%012d", index)
		input := `{"origin":{"client_id":"01900000-0000-7000-8000-000000000900","client_name":"factory","run_id":"01900000-0000-7000-8000-000000000901","project_id":"01900000-0000-7000-8000-000000000902"}}`
		if _, err := database.Exec(`INSERT INTO runs(id,module,kind,state,resources,input,created_at) VALUES(?,?,?,?,?,?,?)`, id, "benchmarks", "benchmark", "queued", "{}", input, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO runs(id,module,kind,state,resources,input,created_at) VALUES(?,?,?,?,?,?,?)`,
		"01900000-0000-7000-8000-999999999999", "benchmarks", "benchmark", "queued", "{}",
		`{"origin":{"client_id":"01900000-0000-7000-8000-000000000999","client_name":"other"}}`, createdAt); err != nil {
		t.Fatal(err)
	}
	service := New(database, queries, events.NewEventBus(queries), t.TempDir())
	filter := BenchmarkOriginFilter{
		ClientID:  "01900000-0000-7000-8000-000000000900",
		RunID:     "01900000-0000-7000-8000-000000000901",
		ProjectID: "01900000-0000-7000-8000-000000000902",
		Limit:     50,
	}
	seen := map[string]struct{}{}
	for {
		page, next, err := service.ListBenchmarkOrigin(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range page {
			if _, duplicate := seen[run.ID]; duplicate {
				t.Fatalf("duplicate run %s", run.ID)
			}
			seen[run.ID] = struct{}{}
		}
		if next == "" {
			break
		}
		filter.Cursor = next
	}
	if len(seen) != 205 {
		t.Fatalf("owned runs = %d, want 205", len(seen))
	}
	if _, _, err := service.ListBenchmarkOrigin(ctx, BenchmarkOriginFilter{Cursor: "malformed"}); err != ErrInvalidCursor {
		t.Fatalf("malformed cursor error = %v", err)
	}
}
