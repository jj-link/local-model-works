package servingtelemetry

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/telemetry"
)

type fakeSource struct {
	deps []deploy.MonitorTarget
}

func (f *fakeSource) ListMonitoringTargets(context.Context) ([]deploy.MonitorTarget, error) {
	return f.deps, nil
}

func openServingStore(t *testing.T) (*telemetry.Service, *sql.DB, *db.Queries) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	q := db.New(database)
	return telemetry.New(database, q), database, q
}

func createDeploymentRow(t *testing.T, database *sql.DB, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `INSERT OR IGNORE INTO recipes (digest, name, version, manifest) VALUES (?,?,?,?)`, "r1", "r", "1", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO deployments (id, recipe_digest, profile, placement, desired_state, observed_state) VALUES (?,?,?,?,?,?)`, id, "r1", "p", id, "running", "healthy"); err != nil {
		t.Fatal(err)
	}
}

func TestCollectFiltersAndPersists(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "vllm:generation_tokens_total 1\n")
	}))
	defer srv.Close()

	tel, database, _ := openServingStore(t)
	for _, id := range []string{"d-healthy", "d-noendpoint", "d-preparing"} {
		createDeploymentRow(t, database, id)
	}
	source := &fakeSource{deps: []deploy.MonitorTarget{
		{ID: "d-healthy", DesiredState: "running", ObservedState: "healthy", Endpoint: deploy.Endpoint{Host: "127.0.0.1", Port: int32(portOf(srv.URL))}},
		{ID: "d-noendpoint", DesiredState: "running", ObservedState: "healthy"},
	}}
	svc := New(source, tel, srv.Client())
	svc.prober.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	if err := svc.Collect(ctx, time.Unix(1_800_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	latest, err := tel.LatestServing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := latest["d-healthy"]; !ok {
		t.Fatalf("healthy not probed: %v", latest)
	}
	for _, skipped := range []string{"d-noendpoint"} {
		if _, ok := latest[skipped]; ok {
			t.Fatalf("skipped deployment %s was probed", skipped)
		}
	}
}

func TestCollectMinuteAggregationAndBatching(t *testing.T) {
	ctx := context.Background()
	tel, database, _ := openServingStore(t)
	createDeploymentRow(t, database, "dep-a")
	createDeploymentRow(t, database, "dep-b")

	base := time.Unix(1_800_000_000, 0)
	if err := tel.IngestServing(ctx, "dep-a", base.Unix(), telemetry.ServingPayload{
		Available: true, Backend: "vllm", ModelID: "m", GenerationTPS: new(float64(10)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tel.IngestServing(ctx, "dep-b", base.Unix(), telemetry.ServingPayload{
		Available: true, Backend: "sglang", ModelID: "s", GenerationTPS: new(float64(20)),
	}); err != nil {
		t.Fatal(err)
	}
	// A second sample for dep-a floors into the same minute.
	if err := tel.IngestServing(ctx, "dep-a", base.Add(5*time.Second).Unix(), telemetry.ServingPayload{
		Available: true, Backend: "vllm", ModelID: "m", GenerationTPS: new(float64(30)),
	}); err != nil {
		t.Fatal(err)
	}

	hist, err := tel.ServingHistory(ctx, "dep-a", "1m", 0, base.Add(time.Minute).Unix(), 100)
	if err != nil || len(hist) != 1 {
		t.Fatalf("hist=%v err=%v", hist, err)
	}
	if *hist[0].Payload.GenerationTPS != 20 {
		t.Fatalf("minute gen=%v want 20", *hist[0].Payload.GenerationTPS)
	}

	latest, err := tel.LatestServing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest count=%d want 2", len(latest))
	}
	if *latest["dep-a"].Payload.GenerationTPS != 30 {
		t.Fatalf("latest dep-a gen=%v want 30", *latest["dep-a"].Payload.GenerationTPS)
	}
}
