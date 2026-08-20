package telemetry

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

func TestIngestResolutionsMetricsAndRetention(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	if err := queries.CreateNode(ctx, db.CreateNodeParams{ID: "node-a", DisplayName: "Node A", Labels: "{}", CreatedAt: "2026-01-01T00:00:00.000Z"}); err != nil {
		t.Fatal(err)
	}
	service := New(database, queries)
	at := time.Unix(1_800_000_005, 0)
	if err := service.Ingest(ctx, "node-a", at, []byte(`{"cpu":{"usage_percent":25},"memory":{"used_bytes":1024}}`)); err != nil {
		t.Fatal(err)
	}
	if err := service.Ingest(ctx, "node-a", at.Add(5*time.Second), []byte(`{"cpu":{"usage_percent":50},"memory":{"used_bytes":2048}}`)); err != nil {
		t.Fatal(err)
	}

	raw, err := service.History(ctx, "node-a", "5s", at.Unix(), at.Add(time.Minute).Unix(), 100)
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw=%v err=%v", raw, err)
	}
	minute, err := service.History(ctx, "node-a", "1m", at.Add(-time.Minute).Unix(), at.Add(time.Minute).Unix(), 100)
	if err != nil || len(minute) != 1 || !strings.Contains(string(minute[0].Payload), `"samples":2`) || !strings.Contains(string(minute[0].Payload), `"used_bytes":1536`) {
		t.Fatalf("minute=%v err=%v", minute, err)
	}
	metrics, err := service.Prometheus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics, `lmw_node_cpu_usage_percent{node_id="node-a"} 50`) || !strings.Contains(metrics, `lmw_node_memory_used_bytes{node_id="node-a"} 2048`) {
		t.Fatalf("metrics:\n%s", metrics)
	}

	if err := service.Prune(ctx, at.Add(MinuteRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	minute, err = service.History(ctx, "node-a", "1m", 0, at.Add(MinuteRetention*2).Unix(), 100)
	if err != nil || len(minute) != 0 {
		t.Fatalf("retained expired samples: %v %v", minute, err)
	}
}
