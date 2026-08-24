// Package backend implements the runs first-party module: the operator's
// view of the run ledger — list, detail, cancel, and byte-cursor log
// streaming. Runs themselves are created by other modules (serving,
// benchmarks, library) through the shared runs service and job SDK; this
// module owns no job kinds.

package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the runs backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// RegisterJobs: the runs module renders runs it does not create.
func (m *Module) RegisterJobs(*jobs.Registry) {}

func (m *Module) RegisterSettings(*settings.Registry) {}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	HandlerFromMux(m, r)
}

// list — GET /runs: newest first, keyset-paginated by created_at (the
// cursor is the previous page's last created_at).
func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := runs.Filter{Limit: 50}
	if v := q.Get("module"); v != "" {
		f.Module = &v
	}
	if v := q.Get("state"); v != "" {
		f.State = &v
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpx.WriteErr(w, http.StatusBadRequest, "invalid.limit", "limit must be a positive integer")
			return
		}
		f.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		f.CreatedBefore = &v
	}
	items, err := m.env.Runs.List(r.Context(), f)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	next := ""
	if len(items) > 0 && len(items) >= f.Limit {
		next = items[len(items)-1].CreatedAt
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "next_cursor": next,
	})
}

// get — GET /runs/{id}
func (m *Module) get(w http.ResponseWriter, r *http.Request) {
	run, err := m.env.Runs.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

// cancel — POST /runs/{id}/cancel
func (m *Module) cancel(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "id")
	if err := m.env.Jobs.Cancel(r.Context(), rid); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, err := m.env.Runs.Get(r.Context(), rid)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

// logs — GET /runs/{id}/logs: SSE byte-cursor stream of the run's stdout
// (the deployment's rank 0, or the ad-hoc stream for one-shot runs).
// Last-Event-ID carries the byte offset to resume from. The stream ends
// with an "end" event once the run is terminal and the log fully drained.
func (m *Module) logs(w http.ResponseWriter, r *http.Request) {
	run, err := m.env.Runs.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	depID := ""
	if run.DeploymentID != nil {
		depID = *run.DeploymentID
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	var offset uint64
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseUint(id, 10, 64); err == nil {
			offset = n
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		chunk, next, size, rerr := m.env.Runs.ReadLog(run.ID, depID, 0, "stdout", offset, 0)
		if rerr != nil {
			return
		}
		if len(chunk) > 0 {
			payload, _ := json.Marshal(string(chunk))
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", next, payload)
			offset = next
			flusher.Flush()
		}
		// Terminate when the run is terminal and the fully drained log is
		// fully delivered (the agent's final marker recorded the EOF).
		if end, done := m.env.Runs.LogEnded(run.ID, depID, 0, "stdout"); done && offset >= size {
			cur, gerr := m.env.Runs.Get(r.Context(), run.ID)
			if gerr == nil && runs.State(cur.State).Terminal() {
				fmt.Fprintf(w, "id: %d\nevent: end\ndata: {\"size\":%d}\n\n", end, size)
				flusher.Flush()
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-poll.C:
		}
	}
}
