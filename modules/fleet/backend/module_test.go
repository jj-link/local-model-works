package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/nodes"
)

func TestUpdateNodeValidatesServePortLabel(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := db.New(sqlDB)
	if err := q.CreateNode(ctx, db.CreateNodeParams{ID: "spark1", DisplayName: "spark1", Labels: `{"serve-port":"8001"}`}); err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{Q: q, Nodes: nodes.NewRegistry()}}
	update := func(body string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPut, "/nodes/spark1", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		routes := chi.NewRouteContext()
		routes.URLParams.Add("id", "spark1")
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routes))
		recorder := httptest.NewRecorder()
		module.updateNode(recorder, request)
		return recorder.Code
	}
	for _, invalid := range []string{"0", "65536", "80a", "08001", " 8001", ""} {
		if code := update(`{"display_name":"spark1","labels":{"serve-port":"` + invalid + `"}}`); code != http.StatusUnprocessableEntity {
			t.Fatalf("serve-port %q: status %d, want 422", invalid, code)
		}
	}
	if node, err := q.GetNode(ctx, "spark1"); err != nil || node.Labels != `{"serve-port":"8001"}` {
		t.Fatalf("rejected update changed labels: %q, %v", node.Labels, err)
	}
	if code := update(`{"display_name":"spark1","labels":{"serve-port":"8004"}}`); code != http.StatusOK {
		t.Fatalf("valid serve-port: status %d", code)
	}
	if port, ok := nodes.LabelServePort(mustLabels(t, q)); !ok || port != 8004 {
		t.Fatalf("stored serve-port = %d, %v", port, ok)
	}
}

func mustLabels(t *testing.T, q *db.Queries) string {
	t.Helper()
	node, err := q.GetNode(context.Background(), "spark1")
	if err != nil {
		t.Fatal(err)
	}
	return node.Labels
}
