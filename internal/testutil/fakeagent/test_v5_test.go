package fakeagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
)

// bootFleet boots a server with n approved online GPU agents
// (spark-<i+1>, 10.0.0.<10+i>/24) and returns the server, agents, node ids.
func bootFleet(t *testing.T, n int) (*Server, []*Agent, []string) {
	t.Helper()
	s := NewServer(t, "", "127.0.0.1:0")
	agents := make([]*Agent, 0, n)
	nodes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tok := s.IssueToken(t)
		a := StartAgent(t, s, AgentOpts{
			Hostname: fmt.Sprintf("spark-%d", i+1),
			Token:    tok,
			IP:       fmt.Sprintf("10.0.0.%d/24", 10+i),
		})
		agents = append(agents, a)
		nodes = append(nodes, a.NodeID())
		s.ApproveNode(t, nodes[i])
		s.WaitOnline(t, nodes[i])
	}
	return s, agents, nodes
}

func hasPlanDiag(p *deploy.Plan, code string) bool {
	for _, d := range p.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestV5_GpuLeaseConflict proves GPU leases are exclusive: a second
// deployment targeting the already-leased GPU gets the 409-equivalent
// ErrNotReady (mapped to HTTP 409 resource.conflict by the serving module,
// module.go:124), the occupying deployment ID is discoverable through the
// lease registry, and a deployment on a different node still fits
// (exclusivity is per-GPU, not per-node).
func TestV5_GpuLeaseConflict(t *testing.T) {
	s, agents, nodes := bootFleet(t, 2)
	n1, n2 := nodes[0], nodes[1]

	digest := install(t, s, FixtureRecipe{Name: "gpu-1r", Version: "1.0.0", NodeCount: 1, GPUsPerRank: 1})
	depA := createDep(t, s, digest)
	depA = waitDep(t, s, depA.ID, "healthy")
	plA := depA.Placements[0]
	if plA.AcceleratorUUID == "" {
		t.Fatalf("deployment A has no accelerator UUID")
	}
	resource := "gpu:" + plA.NodeID + ":" + plA.AcceleratorUUID

	// Same node (hence same GPU) => the plan is not ready and names the
	// accelerator shortage.
	plan, err := s.Srv.Deployments().Plan(s.Ctx, deploy.PlanRequest{
		RecipeDigest: digest,
		Placements:   []deploy.PlacementOverride{{Rank: 0, NodeID: plA.NodeID}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Ready {
		t.Fatalf("plan is ready despite the leased GPU")
	}
	if !hasPlanDiag(plan, "placement.accelerator_unavailable") {
		t.Errorf("plan diagnostics = %v, want placement.accelerator_unavailable", plan.Diagnostics)
	}
	_, err = s.Srv.Deployments().Create(s.Ctx, deploy.CreateRequest{
		RecipeDigest: digest,
		Placements:   []deploy.PlacementOverride{{Rank: 0, NodeID: plA.NodeID}},
	})
	if !errors.Is(err, deploy.ErrNotReady) {
		t.Fatalf("create on leased GPU: err = %v, want ErrNotReady (409)", err)
	}

	// The occupant: the active lease for that exact resource is held by A.
	owners := s.Srv.Runs().ActiveOwners(s.Ctx, resource)
	found := false
	for _, o := range owners {
		if o.OwnerKind == "deployment" && o.OwnerID == depA.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("ActiveOwners(%s) = %v, want deployment %s", resource, owners, depA.ID)
	}
	res := s.Srv.Runs().ResourcesOf(s.Ctx, "deployment", depA.ID)
	if !containsString(res.Accelerators, resource) {
		t.Errorf("ResourcesOf(A).Accelerators = %v, want %s", res.Accelerators, resource)
	}

	// Positive control: the other node's GPU is free, so a second deployment
	// fits there.
	other := n1
	if plA.NodeID == n1 {
		other = n2
	}
	depB := createDep(t, s, digest, deploy.PlacementOverride{Rank: 0, NodeID: other})
	waitDep(t, s, depB.ID, "healthy")
	_ = agents
}

// TestV5_PortConflictCarriesOccupant proves the conflict that carries the
// occupying deployment's ID: two same-node same-port deployments conflict,
// the plan records the occupant deployment ID (openapi conflicts item), and
// Create fails with the 409-equivalent sentinel.
func TestV5_PortConflictCarriesOccupant(t *testing.T) {
	s, _, nodes := bootFleet(t, 1)
	n1 := nodes[0]

	digest := install(t, s, FixtureRecipe{Name: "cpu-port", Version: "1.0.0", NodeCount: 1, Port: 9200})
	depA := createDep(t, s, digest)
	depA = waitDep(t, s, depA.ID, "healthy")
	if depA.Endpoint == nil || depA.Endpoint.Port != 9200 {
		t.Fatalf("deployment A endpoint = %+v, want port 9200", depA.Endpoint)
	}

	plan, err := s.Srv.Deployments().Plan(s.Ctx, deploy.PlanRequest{
		RecipeDigest: digest,
		Placements:   []deploy.PlacementOverride{{Rank: 0, NodeID: n1}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Ready {
		t.Fatalf("plan is ready despite the port conflict")
	}
	if len(plan.Conflicts) == 0 || plan.Conflicts[0].DeploymentID != depA.ID {
		t.Fatalf("plan conflicts = %+v, want occupant deployment %s", plan.Conflicts, depA.ID)
	}

	_, err = s.Srv.Deployments().Create(s.Ctx, deploy.CreateRequest{
		RecipeDigest: digest,
		Placements:   []deploy.PlacementOverride{{Rank: 0, NodeID: n1}},
	})
	if !errors.Is(err, deploy.ErrNotReady) {
		t.Fatalf("create on conflicting port: err = %v, want ErrNotReady (409)", err)
	}
}

// TestV5_StartNeverReplaces proves re-driving an already-started rank
// (agent restart triggers the server's ReconcileRequest + INSPECT path,
// never CREATE) does not replace the container: name and ID unchanged,
// state running, no ghost containers.
func TestV5_StartNeverReplaces(t *testing.T) {
	s, agents, _ := bootFleet(t, 1)
	a1 := agents[0]

	digest := install(t, s, FixtureRecipe{Name: "gpu-1r", Version: "1.0.0", NodeCount: 1, GPUsPerRank: 1})
	dep := createDep(t, s, digest)
	dep = waitDep(t, s, dep.ID, "healthy")
	c := containersOf(a1.RT, dep)[0]
	if c == nil {
		t.Fatalf("rank 0 container missing")
	}
	name, idBefore := c.Name, c.ID

	// Kill the agent; on reconnect the server reconciles and re-drives the
	// rank from its persisted phase (started => INSPECT, not CREATE).
	// The restart returns the live (new) agent instance; a1 is rebound so
	// the reconcile-reason assertion below observes the reconnected agent.
	a1 = a1.Restart(t, "", "")
	s.WaitOnline(t, a1.NodeID())
	Deadline(t, 10*time.Second, func() bool {
		return containsString(a1.ReconcileReasons(), "reconnect")
	}, "reconnect reconcile on the restarted agent")
	waitDep(t, s, dep.ID, "healthy")

	if st := a1.RT.StateOf(name); st != "running" {
		t.Fatalf("container %s state = %s, want running", name, st)
	}
	if idAfter := a1.RT.IDOf(name); idAfter != idBefore {
		t.Fatalf("container id changed %s -> %s: start replaced the container", idBefore, idAfter)
	}
	if n := len(containersOf(a1.RT, dep)); n != 1 {
		t.Errorf("containers for deployment = %d, want 1", n)
	}
	if !containsString(a1.ReconcileReasons(), "reconnect") {
		t.Errorf("reconcile reasons = %v, want reconnect", a1.ReconcileReasons())
	}
}

// TestV5_ServerRestartReconciles proves controller restart recovery for a
// running two-rank deployment: the agents reconnect, the server's reconcile
// re-drives each rank, the containers are NOT recreated (same IDs, still
// running in the agents' stub runtimes), and the deployment converges back
// to healthy.
func TestV5_ServerRestartReconciles(t *testing.T) {
	s, agents, nodes := bootFleet(t, 2)
	a1, a2 := agents[0], agents[1]

	digest := install(t, s, FixtureRecipe{
		Name: "spark-serve-2r", Version: "1.0.0", NodeCount: 2, GPUsPerRank: 1, Port: 8100,
	})
	dep := createDep(t, s, digest)
	dep = waitDep(t, s, dep.ID, "healthy")

	idByNode := map[string]string{}
	for i, pl := range dep.Placements {
		var rt *FakeRuntime
		if pl.NodeID == nodes[0] {
			rt = a1.RT
		} else {
			rt = a2.RT
		}
		c := containersOf(rt, dep)[int32(i)]
		if c == nil {
			t.Fatalf("rank %d container missing before restart", i)
		}
		idByNode[pl.NodeID] = c.ID
	}

	s2 := RestartServer(t, s)
	Deadline(t, 20*time.Second, func() bool {
		return s2.Srv.Nodes().Online(nodes[0]) && s2.Srv.Nodes().Online(nodes[1])
	}, "agents reconnected after server restart")
	dep = waitDep(t, s2, dep.ID, "healthy")

	// Containers were not recreated: same IDs, still running, still in the
	// agents' stub runtimes.
	for i, pl := range dep.Placements {
		var rt *FakeRuntime
		if pl.NodeID == nodes[0] {
			rt = a1.RT
		} else {
			rt = a2.RT
		}
		c := containersOf(rt, dep)[int32(i)]
		if c == nil {
			t.Fatalf("rank %d container gone after server restart", i)
		}
		if c.ID != idByByNode(pl.NodeID, idByNode) {
			t.Fatalf("rank %d container recreated: %s -> %s", i, idByByNode(pl.NodeID, idByNode), c.ID)
		}
		if c.State != "running" {
			t.Errorf("rank %d container state = %s, want running", i, c.State)
		}
	}
	for _, a := range []*Agent{a1, a2} {
		if !containsString(a.ReconcileReasons(), "reconnect") {
			t.Errorf("agent reconcile reasons = %v, want reconnect", a.ReconcileReasons())
		}
	}
}

func idByByNode(nodeID string, m map[string]string) string { return m[nodeID] }

// TestV5_AgentDisconnect proves the designed disconnect semantics: when a
// running rank's agent disappears, its node is marked offline immediately
// (session teardown); the deployment's last reported state is preserved
// (no live channel, so no invented worse state — reported gap vs the plan's
// "unknown/degraded" wording); and when the agent reconnects, the reconcile
// confirms the still-running container and the deployment is healthy again.
func TestV5_AgentDisconnect(t *testing.T) {
	s, agents, nodes := bootFleet(t, 2)
	a1, a2 := agents[0], agents[1]
	n2 := nodes[1]
	_ = a1

	digest := install(t, s, FixtureRecipe{
		Name: "spark-serve-2r", Version: "1.0.0", NodeCount: 2, GPUsPerRank: 1, Port: 8100,
	})
	dep := createDep(t, s, digest)
	dep = waitDep(t, s, dep.ID, "healthy")

	// Disconnect one rank's agent: the node is marked offline.
	a2.Stop()
	Deadline(t, 15*time.Second, func() bool {
		return s.Node(t, n2).Status == "offline"
	}, "disconnected node marked offline")

	Deadline(t, 10*time.Second, func() bool {
		return depState(s, dep.ID) == "degraded"
	}, "multi-rank deployment degraded after one node disconnect")
	leases, err := s.Q.ActiveLeases(s.Ctx)
	if err != nil || len(leases) == 0 {
		t.Fatalf("offline deployment leases were released: %v, err=%v", leases, err)
	}

	// Reconnect: reconcile (reason "reconnect") + the agent's monitor
	// re-reports the still-running container; the deployment is confirmed
	// healthy again. The restart returns the live (new) agent instance;
	// a2 is rebound so the reconcile-reason assertion observes it. The
	// shared stub runtime (same state root) keeps the container alive.
	a2 = a2.Restart(t, "", "")
	s.WaitOnline(t, n2)
	Deadline(t, 10*time.Second, func() bool {
		return containsString(a2.ReconcileReasons(), "reconnect")
	}, "reconnect reconcile on the restarted agent")
	waitDep(t, s, dep.ID, "healthy")
	if st := s.Node(t, n2).Status; st != "online" {
		t.Errorf("node status after reconnect = %s, want online", st)
	}
	if !containsString(a2.ReconcileReasons(), "reconnect") {
		t.Errorf("reconcile reasons = %v, want reconnect", a2.ReconcileReasons())
	}
	// The container survived the whole episode on the agent side.
	for rank, c := range containersOf(a2.RT, depGet(t, s, dep.ID)) {
		if c == nil || c.State != "running" {
			t.Errorf("rank %d container after reconnect = %v, want running", rank, c)
		}
	}
}

func TestV5_SingleRankOfflineBecomesUnknown(t *testing.T) {
	server, agents, _ := bootFleet(t, 1)
	deployment := createDep(t, server, install(t, server, FixtureRecipe{
		Name: "single-offline", Version: "1.0.0", NodeCount: 1,
	}))
	waitDep(t, server, deployment.ID, "healthy")
	agents[0].Stop()
	Deadline(t, 15*time.Second, func() bool {
		return depState(server, deployment.ID) == "unknown"
	}, "single-rank deployment unknown after agent disconnect")
}

// TestV5_LabelScopedStop proves stop is scoped to the exact deployment: two
// deployments on the same node; stopping A stops only A's container (the
// STOP operation resolves the deployment-scoped deterministic name) while B
// keeps running and healthy.
func TestV5_LabelScopedStop(t *testing.T) {
	s, agents, _ := bootFleet(t, 1)
	a1 := agents[0]

	d1 := install(t, s, FixtureRecipe{Name: "cpu-a", Version: "1.0.0", NodeCount: 1})
	d2 := install(t, s, FixtureRecipe{Name: "cpu-b", Version: "1.0.0", NodeCount: 1})
	depA := createDep(t, s, d1)
	depA = waitDep(t, s, depA.ID, "healthy")
	depB := createDep(t, s, d2)
	depB = waitDep(t, s, depB.ID, "healthy")

	cA := containersOf(a1.RT, depA)[0]
	cB := containersOf(a1.RT, depB)[0]
	if cA == nil || cB == nil {
		t.Fatalf("containers missing: A=%v B=%v", cA, cB)
	}

	if _, err := s.Srv.Deployments().Stop(s.Ctx, depA.ID); err != nil {
		t.Fatalf("stop A: %v", err)
	}
	waitDep(t, s, depA.ID, "stopped")

	if st := a1.RT.StateOf(cA.Name); st != "exited" && st != "" {
		t.Errorf("container A state = %q, want exited or removed", st)
	}
	if st := a1.RT.StateOf(cB.Name); st != "running" {
		t.Errorf("container B state = %s, want running", st)
	}
	waitDep(t, s, depB.ID, "healthy")
}

// TestV5_CancelTerminatesExactContainer proves cancellation terminates the
// exact deployment's container: the run reaches the terminal "cancelled"
// state, the stub container for that deployment stops, its leases are
// released, and a sibling deployment on the same node is untouched.
func TestV5_CancelTerminatesExactContainer(t *testing.T) {
	s, agents, _ := bootFleet(t, 1)
	a1 := agents[0]

	d1 := install(t, s, FixtureRecipe{Name: "cpu-a", Version: "1.0.0", NodeCount: 1})
	d2 := install(t, s, FixtureRecipe{Name: "cpu-b", Version: "1.0.0", NodeCount: 1})
	depA := createDep(t, s, d1)
	depA = waitDep(t, s, depA.ID, "healthy")
	depB := createDep(t, s, d2)
	depB = waitDep(t, s, depB.ID, "healthy")

	cA := containersOf(a1.RT, depA)[0]
	cB := containersOf(a1.RT, depB)[0]
	if cA == nil || cB == nil {
		t.Fatalf("containers missing: A=%v B=%v", cA, cB)
	}

	if _, err := s.Srv.Deployments().Stop(s.Ctx, depA.ID); err != nil {
		t.Fatalf("stop (cancel) A: %v", err)
	}
	waitDep(t, s, depA.ID, "stopped")

	run, err := s.Srv.Runs().Get(s.Ctx, depA.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.State != "cancelled" {
		t.Errorf("run state = %s, want cancelled", run.State)
	}
	if st := a1.RT.StateOf(cA.Name); st != "exited" && st != "" {
		t.Errorf("container A state = %q, want exited or removed", st)
	}
	if st := a1.RT.StateOf(cB.Name); st != "running" {
		t.Errorf("container B state = %s, want running", st)
	}
	Deadline(t, 10*time.Second, func() bool {
		res := s.Srv.Runs().ResourcesOf(s.Ctx, "deployment", depA.ID)
		return len(res.Accelerators) == 0 && len(res.Nodes) == 0 && len(res.Fabrics) == 0
	}, "deployment A leases released")
}

// TestV5_LogCursorResume proves the log cursor contract over the real SSE
// wire: bytes already delivered at offset N are never re-sent, new bytes
// arrive in order without gaps, and the stream terminates with the "end"
// event once the run is terminal and the log fully drained (the agent's
// final marker on container exit).
func TestV5_LogCursorResume(t *testing.T) {
	s, agents, _ := bootFleet(t, 1)
	a1 := agents[0]

	digest := install(t, s, FixtureRecipe{Name: "cpu-logs", Version: "1.0.0", NodeCount: 1})
	dep := createDep(t, s, digest)
	dep = waitDep(t, s, dep.ID, "healthy")
	c := containersOf(a1.RT, dep)[0]
	if c == nil {
		t.Fatalf("container missing")
	}

	batch1 := []byte("alpha 0123456789abcdef\nbeta 0123456789abcdef\ngamma 0123456789abcdef\n")
	batch2 := []byte("delta 0123456789abcdef\nepsilon 0123456789abcdef\n")
	a1.RT.WriteLog(c.Name, "stdout", batch1)

	// Wait for the controller's log file to carry exactly batch1.
	Deadline(t, 15*time.Second, func() bool {
		_, _, sz, err := s.Srv.Runs().ReadLog(dep.RunID, dep.ID, 0, "stdout", 0, 0)
		return err == nil && sz == uint64(len(batch1))
	}, "controller log to carry batch1")

	chunk, next, _, err := s.Srv.Runs().ReadLog(dep.RunID, dep.ID, 0, "stdout", 0, 0)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Equal(chunk, batch1) || next != uint64(len(batch1)) {
		t.Fatalf("read log = %q (next %d), want exactly batch1", chunk, next)
	}

	// Open the real SSE stream resumed from Last-Event-ID = len(batch1),
	// then append batch2: the stream must deliver exactly the new bytes.
	// The server replays from Last-Event-ID on the first poll, so the
	// write may land before the stream opens.
	a1.RT.WriteLog(c.Name, "stdout", batch2)
	client := login(t, s)
	N := fmt.Sprintf("%d", len(batch1))
	var got []byte
	readSSE(t, client, s, dep.ID, 0, N, 20*time.Second, func(ev sseEvent) bool {
		if ev.kind == "" {
			var payload string
			if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
				t.Fatalf("sse payload %q: %v", ev.data, err)
			}
			got = append(got, payload...)
		}
		return len(got) >= len(batch2)
	})
	if !bytes.Equal(got, batch2) {
		t.Fatalf("sse resumed stream = %q, want exactly %q (no duplicates, no gaps)", got, batch2)
	}
	if chunk, _, _, err := s.Srv.Runs().ReadLog(dep.RunID, dep.ID, 0, "stdout", uint64(len(batch1)+len(batch2)), 0); err != nil || len(chunk) != 0 {
		t.Fatalf("read log at end offset = %q (err %v), want empty", chunk, err)
	}

	// Stop the deployment: the run is cancelled (terminal), the tailer's
	// clean drain records the final marker, and the stream ends with "end".
	if _, err := s.Srv.Deployments().Stop(s.Ctx, dep.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitDep(t, s, dep.ID, "stopped")
	_, ended := readSSE(t, client, s, dep.ID, 0, N, 30*time.Second, func(sseEvent) bool { return false })
	if !ended {
		t.Fatalf("sse stream did not end with the terminal event")
	}
}
