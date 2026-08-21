// Package backend implements the serving first-party module: the
// deployment lifecycle — plan, create, list, get, stop, verify, and
// byte-cursor log streaming of workload ranks. Container dispatch itself
// lives in the core deploy service (driven on create and on agent
// reconnect); this module renders and commands it.

package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the serving backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// jobInputSchema bounds both job inputs to a single deployment reference.
var jobInputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["deployment_id"],
  "additionalProperties": false,
  "properties": {
    "deployment_id": { "type": "string", "minLength": 1 }
  }
}`)

// serveOutputSchema is the serve job's confirmation.
var serveOutputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["deployment_id", "desired_state", "observed_state"],
  "additionalProperties": false,
  "properties": {
    "deployment_id":  { "type": "string" },
    "desired_state":  { "type": "string" },
    "observed_state": { "type": "string" }
  }
}`)

// RegisterJobs declares the module's job kinds. Both executors are thin
// state operations over the deploy service: the actual container dispatch
// runs in the service's dispatch loop (on create, on stop, and on agent
// reconnect), so the executors never send workload commands themselves.
// A registration failure means a frozen manifest schema failed to
// compile — a wiring bug, not an operator condition.
func (m *Module) RegisterJobs(reg *jobs.Registry) {
	specs := []jobs.Spec{
		{
			Kind:         "serve",
			Title:        "Serve deployment",
			InputSchema:  jobInputSchema,
			OutputSchema: serveOutputSchema,
			Executor:     m.serveJob,
		},
		{
			Kind:        "stop",
			Title:       "Stop deployment",
			InputSchema: jobInputSchema,
			Executor:    m.stopJob,
		},
	}
	for _, spec := range specs {
		if err := reg.Register("serving", spec); err != nil {
			panic(fmt.Sprintf("serving jobs: %v", err))
		}
	}
}

// RegisterSettings declares the operator settings (verify timeout) from
// the manifest's frozen schema; a compile failure is a wiring bug.
func (m *Module) RegisterSettings(reg *settings.Registry) {
	if err := reg.Register("serving", descriptor.SettingsSchema); err != nil {
		panic(fmt.Sprintf("serving settings: %v", err))
	}
}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	HandlerFromMux(m, r)
}

// deployErr maps deploy service sentinels to the stable (status, code)
// pairs the module's OpenAPI fragment declares for /deployments*.
func (m *Module) deployErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, deploy.ErrUnknown):
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", err.Error())
	case errors.Is(err, deploy.ErrRecipe):
		httpx.WriteErr(w, http.StatusNotFound, "recipe.not_found", err.Error())
	case errors.Is(err, deploy.ErrUntrusted):
		httpx.WriteErr(w, http.StatusConflict, "recipe.untrusted", err.Error())
	case errors.Is(err, deploy.ErrProfile), errors.Is(err, deploy.ErrNoTarget):
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
	case errors.Is(err, deploy.ErrPlanStale):
		httpx.WriteErr(w, http.StatusConflict, "plan.stale", err.Error())
	case errors.Is(err, deploy.ErrNotReady), errors.Is(err, deploy.ErrConflict), errors.Is(err, deploy.ErrState):
		httpx.WriteErr(w, http.StatusConflict, "resource.conflict", err.Error())
	default:
		httpx.HandleErr(w, err)
	}
}

// plan — POST /deployments/plan: preview placement, artifacts, ports,
// risks, conflicts.
func (m *Module) plan(w http.ResponseWriter, r *http.Request) {
	var req deploy.PlanRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	p, err := m.env.Deploy.Plan(r.Context(), req)
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

// create — POST /deployments: validate the plan digest, commit the
// deployment with its resource leases, and start dispatch.
func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	var req deploy.CreateRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	dep, err := m.env.Deploy.Create(r.Context(), req)
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, dep)
}

// list — GET /deployments.
func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	deps, err := m.env.Deploy.List(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, deps)
}

// get — GET /deployments/{id}.
func (m *Module) get(w http.ResponseWriter, r *http.Request) {
	dep, err := m.env.Deploy.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dep)
}

// stop — POST /deployments/{id}/stop: stop exactly this deployment's
// workload (label-scoped). Offline ranks keep their leases and are
// re-driven on reconnect.
func (m *Module) stop(w http.ResponseWriter, r *http.Request) {
	dep, err := m.env.Deploy.Stop(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dep)
}

// start — POST /deployments/{id}/start: restart a fully-stopped deployment
// (re-acquire leases and re-dispatch), making stop reversible.
func (m *Module) start(w http.ResponseWriter, r *http.Request) {
	dep, err := m.env.Deploy.Start(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dep)
}

// deleteDeployment — DELETE /deployments/{id}: remove a fully-stopped
// deployment and its runs, freeing the recipe/placement slot.
func (m *Module) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if err := m.env.Deploy.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		m.deployErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// verify — POST /deployments/{id}/verify: run the workload probe against
// the live endpoint.
func (m *Module) verify(w http.ResponseWriter, r *http.Request) {
	dep, err := m.env.Deploy.Verify(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dep)
}

// logs — GET /deployments/{id}/logs: SSE byte-cursor stream of one rank's
// workload stdout. The query `rank` selects the rank (default 0); the log
// files are keyed by the deployment's run. Last-Event-ID carries the byte
// offset to resume from. The stream ends with an "end" event once the run
// is terminal and the rank log is fully drained (the agent's final marker
// recorded the EOF).
func (m *Module) logs(w http.ResponseWriter, r *http.Request) {
	depID := chi.URLParam(r, "id")
	dep, err := m.env.Deploy.Get(r.Context(), depID)
	if err != nil {
		m.deployErr(w, err)
		return
	}
	if dep.RunID == "" {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found",
			"deployment has no run yet")
		return
	}
	rank := int32(0)
	if v := r.URL.Query().Get("rank"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpx.WriteErr(w, http.StatusBadRequest, "invalid.rank",
				"rank must be a non-negative integer")
			return
		}
		rank = int32(n)
	}
	// The runs service owns the log path layout; the pre-stream check keeps
	// the path-escape validation in effect for any component before the
	// stream opens (a bad id is a 404, not a broken stream).
	if m.env.Runs.LogPath(dep.RunID, depID, rank, "stdout") == "" {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "invalid log path")
		return
	}
	if _, err := m.env.Runs.Get(r.Context(), dep.RunID); err != nil {
		httpx.HandleErr(w, err)
		return
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
		chunk, next, size, rerr := m.env.Runs.ReadLog(dep.RunID, depID, rank, "stdout", offset, 0)
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
		if end, done := m.env.Runs.LogEnded(dep.RunID, depID, rank, "stdout"); done && offset >= size {
			cur, gerr := m.env.Runs.Get(r.Context(), dep.RunID)
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

// deploymentIDFromInput extracts the job input's deployment reference.
func deploymentIDFromInput(c *jobs.Context) (string, error) {
	depID, ok := c.Input["deployment_id"].(string)
	if !ok || depID == "" {
		return "", fmt.Errorf("%w: deployment_id required", jobs.ErrInput)
	}
	return depID, nil
}

// serveJob confirms a deployment is being served. The actual container
// dispatch is driven by the deploy service's dispatch loop (on create and
// on agent reconnect), so this executor performs a state check and reports
// the observed state rather than sending workload commands itself.
func (m *Module) serveJob(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	depID, err := deploymentIDFromInput(c)
	if err != nil {
		return nil, err
	}
	dep, err := m.env.Deploy.Get(ctx, depID)
	if err != nil {
		return nil, err
	}
	if dep.DesiredState != "running" {
		return nil, fmt.Errorf("%w: deployment is %s", deploy.ErrState, dep.DesiredState)
	}
	c.Logf("deployment %s: desired %s, observed %s", dep.ID, dep.DesiredState, dep.ObservedState)
	return map[string]any{
		"deployment_id":  dep.ID,
		"desired_state":  dep.DesiredState,
		"observed_state": dep.ObservedState,
	}, nil
}

// stopJob issues a stop for exactly the named deployment (label-scoped)
// and returns the resulting deployment view. Offline ranks keep their
// leases and are re-driven on reconnect, as with the stop endpoint.
func (m *Module) stopJob(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	depID, err := deploymentIDFromInput(c)
	if err != nil {
		return nil, err
	}
	dep, err := m.env.Deploy.Stop(ctx, depID)
	if err != nil {
		return nil, err
	}
	c.Logf("deployment %s stopping (observed %s)", dep.ID, dep.ObservedState)
	b, err := json.Marshal(dep)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
