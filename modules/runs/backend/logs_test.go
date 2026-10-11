package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
)

func TestRunLogsWaitForMissingEOFBytesAndPreserveStringEvents(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	q := db.New(database)
	service := runs.New(database, q, events.NewEventBus(q), t.TempDir())
	runID, err := service.Create(ctx, "runs", "build", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []runs.State{runs.Running, runs.Verifying, runs.Succeeded} {
		if err := service.SetState(ctx, runID, state, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	handler := Handler(&Module{env: &moduleapi.Env{Runs: service}})
	request := func(cursor string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		r := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/logs", nil).WithContext(ctx)
		r.Header.Set("Last-Event-ID", cursor)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if err := service.AppendLog(runID, "", 0, "stdout", []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	service.MarkLogEnd(runID, "", 0, "stdout", 11)
	if body := request("0"); !strings.Contains(body, `data: "first\n"`) || strings.Contains(body, "event: end") {
		t.Fatalf("premature EOF or changed single-stream contract: %s", body)
	}
	if err := service.AppendLog(runID, "", 0, "stdout", []byte("last\n")); err != nil {
		t.Fatal(err)
	}
	if body := request("6"); !strings.Contains(body, `data: "last\n"`) || !strings.Contains(body, "event: end") || strings.Contains(body, "first") {
		t.Fatalf("resumed single-stream output lost or duplicated: %s", body)
	}
	if body := request("9999"); !strings.Contains(body, "id: 11\nevent: snapshot") || !strings.Contains(body, `data: "first\nlast\n"`) || !strings.Contains(body, "event: end") {
		t.Fatalf("stale single-stream cursor did not recover: %s", body)
	}
}
