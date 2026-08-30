package telemetry

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

func openTestService(t *testing.T, nodeIDs ...string) *Service {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.New(database)
	for _, id := range nodeIDs {
		if err := queries.CreateNode(ctx, db.CreateNodeParams{ID: id, DisplayName: id, Labels: "{}", CreatedAt: "2026-01-01T00:00:00.000Z"}); err != nil {
			t.Fatal(err)
		}
	}
	return New(database, queries)
}

func basePayload(usage uint32, used uint64) NodePayload {
	return NodePayload{
		CPU:           &CPUPayload{UsagePercent: usage, Cores: 10},
		Memory:        &MemoryPayload{UsedBytes: used, TotalBytes: 1 << 40},
		UptimeSeconds: 100,
		Network:       &NetworkPayload{RxBytesPerSecond: 5, TxBytesPerSecond: 3},
	}
}

func TestIngestNodeResolutionsMetricsAndRetention(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t, "node-a")
	at := time.Unix(1_800_000_005, 0)
	if err := svc.IngestNode(ctx, "node-a", at, basePayload(25, 1024)); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestNode(ctx, "node-a", at.Add(5*time.Second), basePayload(50, 2048)); err != nil {
		t.Fatal(err)
	}

	raw, err := svc.NodeHistory(ctx, "node-a", "5s", at.Unix(), at.Add(time.Minute).Unix(), 100)
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw=%v err=%v", raw, err)
	}
	if raw[0].Payload.CPU.UsagePercent != 25 || raw[1].Payload.CPU.UsagePercent != 50 {
		t.Fatalf("raw usage: %+v", raw)
	}

	minute, err := svc.NodeHistory(ctx, "node-a", "1m", at.Add(-time.Minute).Unix(), at.Add(time.Minute).Unix(), 100)
	if err != nil || len(minute) != 1 {
		t.Fatalf("minute=%v err=%v", minute, err)
	}
	if minute[0].Payload.CPU.UsagePercent != 37 {
		t.Fatalf("minute usage=%d want 37", minute[0].Payload.CPU.UsagePercent)
	}
	if minute[0].Payload.Memory.UsedBytes != 1536 {
		t.Fatalf("minute mem=%d want 1536", minute[0].Payload.Memory.UsedBytes)
	}
	if minute[0].Payload.Network.RxBytesPerSecond != 5 {
		t.Fatalf("minute net rx=%d want 5", minute[0].Payload.Network.RxBytesPerSecond)
	}

	latest, err := svc.LatestNodes(ctx)
	if err != nil || len(latest) != 1 {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
	if latest["node-a"].Payload.CPU.UsagePercent != 50 {
		t.Fatalf("latest usage=%d want 50", latest["node-a"].Payload.CPU.UsagePercent)
	}

	metrics, err := svc.Prometheus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics, `lmw_node_cpu_usage_percent{node_id="node-a"} 50`) {
		t.Fatalf("metrics missing cpu:\n%s", metrics)
	}
	if !strings.Contains(metrics, `lmw_node_memory_used_bytes{node_id="node-a"} 2048`) {
		t.Fatalf("metrics missing memory:\n%s", metrics)
	}

	if err := svc.Prune(ctx, at.Add(MinuteRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	minute, err = svc.NodeHistory(ctx, "node-a", "1m", 0, at.Add(time.Minute*2).Unix(), 100)
	if err != nil || len(minute) != 0 {
		t.Fatalf("retained expired samples: %v %v", minute, err)
	}
}

func TestNodeZeroValuesPreserved(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t, "node-z")
	at := time.Unix(1_800_000_005, 0)
	// A valid zero utilization/rate sample must survive the round trip.
	p := NodePayload{
		CPU:     &CPUPayload{UsagePercent: 0, Cores: 10},
		Memory:  &MemoryPayload{UsedBytes: 0, TotalBytes: 0},
		Network: &NetworkPayload{RxBytesPerSecond: 0, TxBytesPerSecond: 0},
	}
	if err := svc.IngestNode(ctx, "node-z", at, p); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.LatestNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := latest["node-z"].Payload
	if got.CPU == nil || got.Memory == nil || got.Network == nil {
		t.Fatalf("nil blocks: %+v", got)
	}
	metrics, err := svc.Prometheus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics, `lmw_node_cpu_usage_percent{node_id="node-z"} 0`) {
		t.Fatalf("zero usage not emitted:\n%s", metrics)
	}
}

func TestLegacyMinuteNormalization(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t, "node-l")
	q := db.New(svc.db)
	// Insert a pre-upgrade minute row shaped as {"samples":…,"average":…,"last":…}.
	legacy := `{"samples":2,"average":{"cpu":{"usage_percent":40,"cores":10},"memory":{"used_bytes":4096,"total_bytes":8192}},"last":{"cpu":{"usage_percent":50,"cores":10}}}`
	if err := q.InsertTelemetry1m(ctx, db.InsertTelemetry1mParams{NodeID: "node-l", Ts: 1_800_000_000, Payload: legacy}); err != nil {
		t.Fatal(err)
	}
	hist, err := svc.NodeHistory(ctx, "node-l", "1m", 0, 1_800_000_060, 100)
	if err != nil || len(hist) != 1 {
		t.Fatalf("hist=%v err=%v", hist, err)
	}
	if hist[0].Payload.CPU.UsagePercent != 40 {
		t.Fatalf("normalized usage=%d want 40", hist[0].Payload.CPU.UsagePercent)
	}
}

func TestLatestNodesBatching(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t, "node-1", "node-2")
	at := time.Unix(1_800_000_005, 0)
	for _, id := range []string{"node-1", "node-2"} {
		if err := svc.IngestNode(ctx, id, at, basePayload(10, 100)); err != nil {
			t.Fatal(err)
		}
	}
	// node-2 sends a newer sample.
	if err := svc.IngestNode(ctx, "node-2", at.Add(15*time.Second), basePayload(90, 999)); err != nil {
		t.Fatal(err)
	}
	latest, err := svc.LatestNodes(ctx)
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
	if latest["node-1"].Payload.CPU.UsagePercent != 10 || latest["node-2"].Payload.CPU.UsagePercent != 90 {
		t.Fatalf("batch wrong: %+v", latest)
	}
}

func TestIngestServingHistoryAndAggregation(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t)
	// serving_telemetry_* reference deployments(id); provision a recipe + row.
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO recipes (digest, name, version, manifest) VALUES (?,?,?,?)`, "r1", "r", "1", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO deployments (id, recipe_digest, profile, placement, desired_state, observed_state) VALUES (?,?,?,?,?,?)`, "dep-1", "r1", "p", "{}", "stopped", "stopped"); err != nil {
		t.Fatal(err)
	}
	at := int64(1_800_000_005)
	gen := float64(10)
	pre := float64(4)
	first := ServingPayload{Available: true, Backend: "vllm", ModelID: "m", GenerationTPS: &gen, PrefillTPS: &pre, RequestsRunning: 2}
	gen2 := float64(10)
	second := ServingPayload{Available: true, Backend: "vllm", ModelID: "m", GenerationTPS: &gen2, PrefillTPS: &pre, RequestsRunning: 2}
	if err := svc.IngestServing(ctx, "dep-1", at, first); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestServing(ctx, "dep-1", at+5, second); err != nil {
		t.Fatal(err)
	}

	hist, err := svc.ServingHistory(ctx, "dep-1", "5s", at-5, at+60, 100)
	if err != nil || len(hist) != 2 {
		t.Fatalf("hist=%v err=%v", hist, err)
	}
	minute, err := svc.ServingHistory(ctx, "dep-1", "1m", at-60, at+120, 100)
	if err != nil || len(minute) != 1 {
		t.Fatalf("minute=%v err=%v", minute, err)
	}
	if *minute[0].Payload.GenerationTPS != 10 {
		t.Fatalf("minute gen=%v want 10", *minute[0].Payload.GenerationTPS)
	}
	latest, err := svc.LatestServing(ctx)
	if err != nil || len(latest) != 1 {
		t.Fatalf("latest=%v err=%v", latest, err)
	}
	if latest["dep-1"].Payload.Backend != "vllm" {
		t.Fatalf("latest backend=%v", latest["dep-1"].Payload.Backend)
	}
	metrics, err := svc.Prometheus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics, `lmw_serving_generation_tps{deployment_id="dep-1"} 10`) {
		t.Fatalf("serving metric missing:\n%s", metrics)
	}
}

// TestHistoryLimitBounds proves the one-minute history limits accept the
// seven-day contract (10080) while out-of-range values (100001) fall back to
// the 2000 default, for both node and serving history.
func TestHistoryLimitBounds(t *testing.T) {
	ctx := context.Background()
	svc := openTestService(t, "node-l")
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO recipes (digest, name, version, manifest) VALUES (?,?,?,?)`, "r-limit", "r", "1", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.ExecContext(ctx, `INSERT INTO deployments (id, recipe_digest, profile, placement, desired_state, observed_state) VALUES (?,?,?,?,?,?)`, "dep-limit", "r-limit", "p", "{}", "stopped", "stopped"); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0)
	p := basePayload(1, 1)
	for i := 0; i < 2005; i++ {
		at := base.Add(time.Duration(i) * 5 * time.Second)
		if err := svc.IngestNode(ctx, "node-l", at, p); err != nil {
			t.Fatal(err)
		}
		if err := svc.IngestServing(ctx, "dep-limit", at.Unix(), ServingPayload{Available: true, Backend: "vllm", ModelID: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	to := base.Add(time.Duration(2005) * 5 * time.Second).Unix()

	nodeOK, err := svc.NodeHistory(ctx, "node-l", "5s", base.Unix(), to, 10080)
	if err != nil || len(nodeOK) != 2005 {
		t.Fatalf("node limit=10080: n=%d err=%v", len(nodeOK), err)
	}
	nodeFB, err := svc.NodeHistory(ctx, "node-l", "5s", base.Unix(), to, 100001)
	if err != nil || len(nodeFB) != 2000 {
		t.Fatalf("node limit=100001 fallback: n=%d err=%v", len(nodeFB), err)
	}

	servOK, err := svc.ServingHistory(ctx, "dep-limit", "5s", base.Unix(), to, 10080)
	if err != nil || len(servOK) != 2005 {
		t.Fatalf("serving limit=10080: n=%d err=%v", len(servOK), err)
	}
	servFB, err := svc.ServingHistory(ctx, "dep-limit", "5s", base.Unix(), to, 100001)
	if err != nil || len(servFB) != 2000 {
		t.Fatalf("serving limit=100001 fallback: n=%d err=%v", len(servFB), err)
	}
}
