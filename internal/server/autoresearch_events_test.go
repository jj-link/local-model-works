package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestAutoResearchEventChunksPersistInvocation(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	if _, err := database.ExecContext(ctx, `
INSERT INTO autoresearch_projects(id,name,status,config_json) VALUES('project','Project','running','{}');
INSERT INTO runs(id,module,kind,state,resources,input,created_at) VALUES('run','autoresearch','autoresearch-factory','running','{}','{}','now');
INSERT INTO autoresearch_runs(run_id,project_id,factory,config_snapshot) VALUES('run','project','idea','{}');`); err != nil {
		t.Fatal(err)
	}
	server := &Server{q: queries, bus: events.NewEventBus(queries), eventBufs: map[string][]byte{}}
	started := `{"version":1,"event_id":"e1","run_id":"run","invocation_id":"inv","timestamp":"2026-01-01T00:00:00Z","type":"agent.started","payload":{"role":"idea-creator","backend":"codex","model":"fixture"}}` + "\n"
	server.publishAutoResearchEvents("", "run", []byte(started[:37]))
	server.publishAutoResearchEvents("", "run", []byte(started[37:]))
	usage := `{"version":1,"event_id":"e2","run_id":"run","invocation_id":"inv","timestamp":"2026-01-01T00:00:01Z","type":"agent.usage","payload":{"input_tokens":7,"output_tokens":3,"cost_usd":0.25}}` + "\n"
	finished := `{"version":1,"event_id":"e3","run_id":"run","invocation_id":"inv","timestamp":"2026-01-01T00:00:02Z","type":"agent.finished","payload":{"ok":true}}` + "\n"
	server.publishAutoResearchEvents("", "run", []byte(usage+finished))
	row, err := queries.GetAutoResearchInvocation(ctx, "inv")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "completed" || row.InputTokens != 7 || row.OutputTokens != 3 || row.CostUsd != 0.25 {
		t.Fatalf("invocation = %+v", row)
	}
}
