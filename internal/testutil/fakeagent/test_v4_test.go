package fakeagent

import (
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/inventory"
)

// TestV4_Enrollment proves the enrollment protocol end to end over the real
// mTLS/Connect stack: two agents enroll with distinct one-use tokens, both
// are approved and reach online with NVIDIA-like inventory visible through
// the generic inventory schema; token reuse and expired tokens are rejected.
func TestV4_Enrollment(t *testing.T) {
	s := NewServer(t, "", "127.0.0.1:0")

	tok1 := s.IssueToken(t)
	a1 := StartAgent(t, s, AgentOpts{Hostname: "spark-1", Token: tok1, IP: "10.0.0.11/24", RDMA: true})
	n1 := a1.NodeID()
	tok2 := s.IssueToken(t)
	a2 := StartAgent(t, s, AgentOpts{Hostname: "spark-2", Token: tok2, IP: "10.0.0.12/24", RDMA: true})
	n2 := a2.NodeID()
	if n1 == n2 {
		t.Fatalf("distinct identities expected, both %s", n1)
	}
	_ = a1
	_ = a2

	s.ApproveNode(t, n1)
	s.ApproveNode(t, n2)
	row1 := s.WaitOnline(t, n1)
	row2 := s.WaitOnline(t, n2)

	// Inventory: NVIDIA-like, through the generic schema.
	inv, err := inventory.Parse(row1.Inventory.String)
	if err != nil {
		t.Fatalf("parse inventory: %v", err)
	}
	if len(inv.Accelerators) != 1 {
		t.Fatalf("accelerators = %d, want 1", len(inv.Accelerators))
	}
	g := inv.Accelerators[0]
	if g.Vendor != "nvidia" || g.Architecture != "sm_120" {
		t.Errorf("accelerator vendor/arch = %s/%s, want nvidia/sm_120", g.Vendor, g.Architecture)
	}
	if !strings.Contains(g.Name, "Spark") || g.MemoryBytes != 128<<30 {
		t.Errorf("accelerator name/memory = %s/%d, want Spark-class 128GiB", g.Name, g.MemoryBytes)
	}
	if g.UUID != "GPU-spark-1-00000000000000000000000000000000" {
		t.Errorf("accelerator UUID = %s", g.UUID)
	}
	if !inv.HasInterface("lmw-eth0") {
		t.Errorf("lmw-eth0 interface missing from inventory")
	}
	ethOK := false
	for _, a := range inv.InterfaceAddresses("lmw-eth0") {
		if a == "10.0.0.11" {
			ethOK = true
		}
	}
	if !ethOK {
		t.Errorf("lmw-eth0 addresses = %v, want 10.0.0.11", inv.InterfaceAddresses("lmw-eth0"))
	}
	if !inv.HasRdmaDevice("mlx5_0") {
		t.Errorf("mlx5_0 RDMA device missing from inventory")
	}
	// The second node's address must be its own (no cross-contamination).
	inv2, err := inventory.Parse(row2.Inventory.String)
	if err != nil {
		t.Fatalf("parse inventory 2: %v", err)
	}
	if len(inv2.InterfaceAddresses("lmw-eth0")) != 1 || inv2.InterfaceAddresses("lmw-eth0")[0] != "10.0.0.12" {
		t.Errorf("node2 lmw-eth0 = %v, want 10.0.0.12", inv2.InterfaceAddresses("lmw-eth0"))
	}

	// Token reuse: enrollment tokens are one-use (internal/auth + server
	// agent.go:142 "enrollment token already used"). A second agent with the
	// same token is rejected and no node row is created.
	a3 := StartAgent(t, s, AgentOpts{Hostname: "spark-dup", Token: tok2, IP: "10.0.0.13/24"})
	err = a3.RunError(15 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("token reuse: RunError = %v, want 'already used'", err)
	}

	// Expired tokens are rejected (server agent.go:146).
	tokE := s.IssueTokenExpiring(t, time.Now().Add(-time.Minute))
	a4 := StartAgent(t, s, AgentOpts{Hostname: "spark-exp", Token: tokE, IP: "10.0.0.14/24"})
	err = a4.RunError(15 * time.Second)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token: RunError = %v, want 'expired'", err)
	}

	// No phantom node rows from the rejected enrollments.
	nodes, err := s.Q.ListNodes(s.Ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (rejected enrollments must not create rows)", len(nodes))
	}
}

// TestV4_Fabric proves fabric validation against stubbed per-member RoCE
// bindings: complete interface/address/device/GID wiring validates, while a
// missing device on either member leaves the fabric incomplete.
func TestV4_Fabric(t *testing.T) {
	s := NewServer(t, "", "127.0.0.1:0")
	fabs := s.Srv.Env().Fabrics

	tok1, tok2, tok3 := s.IssueToken(t), s.IssueToken(t), s.IssueToken(t)
	a1 := StartAgent(t, s, AgentOpts{Hostname: "spark-1", Token: tok1, IP: "10.0.0.11/24", RDMA: true})
	a2 := StartAgent(t, s, AgentOpts{Hostname: "spark-2", Token: tok2, IP: "10.0.0.12/24", RDMA: true})
	a3 := StartAgent(t, s, AgentOpts{Hostname: "spark-3", Token: tok3, IP: "10.0.0.13/24", RDMA: false})
	n1, n2, n3 := a1.NodeID(), a2.NodeID(), a3.NodeID()
	for _, n := range []string{n1, n2, n3} {
		s.ApproveNode(t, n)
		s.WaitOnline(t, n)
	}
	gid := int32(3)
	binding := func(nodeID, address string) fabric.MemberBinding {
		return fabric.MemberBinding{
			NodeID: nodeID, InterfaceName: "lmw-eth0", Address: address,
			RDMADevice: "mlx5_0", GIDIndex: &gid,
		}
	}

	f, err := fabs.Create(s.Ctx, fabric.CreateRequest{
		Name: "spark-p2p", Transport: "roce",
		Members:  []string{n1, n2},
		Bindings: []fabric.MemberBinding{binding(n1, "10.0.0.11"), binding(n2, "10.0.0.12")},
	})
	if err != nil {
		t.Fatalf("create fabric: %v", err)
	}
	if f.State != "ok" {
		t.Fatalf("fabric state = %s (diags %v), want ok", f.State, f.Diagnostics)
	}

	// A member without the configured RDMA device blocks the fabric.
	state, ds, err := fabs.Validate(s.Ctx, "", fabric.CreateRequest{
		Name: "mixed", Transport: "roce",
		Members:  []string{n1, n3},
		Bindings: []fabric.MemberBinding{binding(n1, "10.0.0.11"), binding(n3, "10.0.0.13")},
	})
	if err != nil {
		t.Fatalf("validate mixed: %v", err)
	}
	if state != "incomplete" {
		t.Errorf("mixed state = %s, want incomplete", state)
	}
	if !hasDiagCode(ds, "fabric.member_no_rdma") {
		t.Errorf("mixed diags = %v, want fabric.member_no_rdma", ds)
	}

	// RoCE bindings without device names are a hard error.
	first, second := binding(n1, "10.0.0.11"), binding(n2, "10.0.0.12")
	first.RDMADevice, second.RDMADevice = "", ""
	state, ds, err = fabs.Validate(s.Ctx, "", fabric.CreateRequest{
		Name: "nodev", Transport: "roce",
		Members:  []string{n1, n2},
		Bindings: []fabric.MemberBinding{first, second},
	})
	if err != nil {
		t.Fatalf("validate nodev: %v", err)
	}
	if state != "incomplete" || !hasDiagCode(ds, "fabric.roce_requires_device") {
		t.Errorf("nodev state/diags = %s / %v, want incomplete + fabric.roce_requires_device", state, ds)
	}
}

func hasDiagCode(ds []diag.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestV4_ServerRestart proves agents reconnect after a controller restart
// (same state root): the new server's in-memory node registry repopulates
// from the reconnecting sessions, and node identities survive.
func TestV4_ServerRestart(t *testing.T) {
	s := NewServer(t, "", "127.0.0.1:0")
	tok1, tok2 := s.IssueToken(t), s.IssueToken(t)
	a1 := StartAgent(t, s, AgentOpts{Hostname: "spark-1", Token: tok1, IP: "10.0.0.11/24"})
	a2 := StartAgent(t, s, AgentOpts{Hostname: "spark-2", Token: tok2, IP: "10.0.0.12/24"})
	_ = a1
	_ = a2
	n1, n2 := a1.NodeID(), a2.NodeID()
	s.ApproveNode(t, n1)
	s.ApproveNode(t, n2)
	s.WaitOnline(t, n1)
	s.WaitOnline(t, n2)

	s2 := RestartServer(t, s)
	// The fresh server's registry is empty until the agents re-dial.
	Deadline(t, 20*time.Second, func() bool {
		return s2.Srv.Nodes().Online(n1) && s2.Srv.Nodes().Online(n2)
	}, "both agents reconnected to the restarted server")

	// Identities and persisted state survived the restart.
	if row := s2.Node(t, n1); row.ID != n1 || row.Status != "online" {
		t.Errorf("node1 after restart = %s/%s", row.ID, row.Status)
	}
	if row := s2.Node(t, n2); row.ID != n2 || row.Status != "online" {
		t.Errorf("node2 after restart = %s/%s", row.ID, row.Status)
	}
}

// TestV4_TwoRankSchedule schedules a minimal valid two-rank recipe across
// the two enrolled GPU nodes and proves the full dispatch path on the stub
// runtime: digest-pinned pull, deterministic container names, both ranks
// running, deployment healthy, endpoint assigned.
func TestV4_TwoRankSchedule(t *testing.T) {
	s := NewServer(t, "", "127.0.0.1:0")
	tok1, tok2 := s.IssueToken(t), s.IssueToken(t)
	a1 := StartAgent(t, s, AgentOpts{Hostname: "spark-1", Token: tok1, IP: "10.0.0.11/24", RDMA: true})
	a2 := StartAgent(t, s, AgentOpts{Hostname: "spark-2", Token: tok2, IP: "10.0.0.12/24", RDMA: true})
	_ = a1
	_ = a2
	n1, n2 := a1.NodeID(), a2.NodeID()
	s.ApproveNode(t, n1)
	s.ApproveNode(t, n2)
	s.WaitOnline(t, n1)
	s.WaitOnline(t, n2)

	digest := install(t, s, FixtureRecipe{
		Name: "spark-serve-2r", Version: "1.0.0", NodeCount: 2, GPUsPerRank: 1, Port: 8100,
	})
	dep := createDep(t, s, digest)
	dep = waitDep(t, s, dep.ID, "healthy")

	if len(dep.Placements) != 2 {
		t.Fatalf("placements = %d, want 2", len(dep.Placements))
	}
	// Two one-GPU nodes => the two ranks land on distinct nodes.
	if dep.Placements[0].NodeID == dep.Placements[1].NodeID {
		t.Errorf("ranks share node %s, want distinct", dep.Placements[0].NodeID)
	}
	for i, pl := range dep.Placements {
		if pl.NodeID != n1 && pl.NodeID != n2 {
			t.Errorf("rank %d on unknown node %s", i, pl.NodeID)
		}
		if pl.AcceleratorUUID == "" {
			t.Errorf("rank %d has no accelerator UUID", i)
		}
	}

	// Both containers exist on the correct stub runtimes, running.
	var ranOn [2]*FakeRuntime
	ranOn[0], ranOn[1] = a1.RT, a2.RT
	for i, pl := range dep.Placements {
		var rt *FakeRuntime
		if pl.NodeID == n1 {
			rt = a1.RT
		} else {
			rt = a2.RT
		}
		cs := containersOf(rt, dep)
		if c, ok := cs[int32(i)]; !ok || c == nil || c.State != "running" {
			t.Errorf("rank %d container missing/not running on node %s (have %v)", i, pl.NodeID, rt.Containers())
		}
	}

	// Pulls are digest-pinned (the validator rejects mutable tags).
	for _, rt := range []*FakeRuntime{a1.RT, a2.RT} {
		pinned := false
		for _, p := range rt.Pulls() {
			if p == ImageServe+"@"+ImageServeDig {
				pinned = true
			}
		}
		if !pinned {
			t.Errorf("pulls = %v, want %s@%s", rt.Pulls(), ImageServe, ImageServeDig)
		}
	}

	// Endpoint: rank 0's controller-facing inventory address + base port
	// (host port = 8100 + rank). Tailscale is preferred when the test host
	// reports it, so assert inventory membership rather than one fake NIC.
	if dep.Endpoint == nil {
		t.Fatalf("no endpoint assigned")
	}
	head, err := s.Q.GetNode(s.Ctx, dep.Placements[0].NodeID)
	if err != nil {
		t.Fatal(err)
	}
	headInventory, err := inventory.Parse(head.Inventory.String)
	if err != nil {
		t.Fatal(err)
	}
	hostOK := false
	for _, network := range headInventory.Interfaces {
		for _, address := range network.Addresses {
			hostOK = hostOK || strings.Split(address, "/")[0] == dep.Endpoint.Host
		}
	}
	if !hostOK {
		t.Errorf("endpoint host = %s, want a rank-0 node interface address", dep.Endpoint.Host)
	}
	if dep.Endpoint.Port != 8100 {
		t.Errorf("endpoint port = %d, want 8100", dep.Endpoint.Port)
	}
}

// TestV4_ThirdNodeCompatible proves a newly enrolled node immediately
// appears in the fleet and can host recipes with no config or code
// regeneration: while the two-rank GPU deployment runs, a fresh third node
// is approved and one rank of a CPU two-rank recipe is pinned to it.
func TestV4_ThirdNodeCompatible(t *testing.T) {
	s := NewServer(t, "", "127.0.0.1:0")
	tok1, tok2 := s.IssueToken(t), s.IssueToken(t)
	a1 := StartAgent(t, s, AgentOpts{Hostname: "spark-1", Token: tok1, IP: "10.0.0.11/24"})
	a2 := StartAgent(t, s, AgentOpts{Hostname: "spark-2", Token: tok2, IP: "10.0.0.12/24"})
	_ = a1
	_ = a2
	n1, n2 := a1.NodeID(), a2.NodeID()
	s.ApproveNode(t, n1)
	s.ApproveNode(t, n2)
	s.WaitOnline(t, n1)
	s.WaitOnline(t, n2)

	// The two-rank GPU deployment occupies both original nodes.
	gpuDigest := install(t, s, FixtureRecipe{
		Name: "spark-serve-2r", Version: "1.0.0", NodeCount: 2, GPUsPerRank: 1, Port: 8100,
	})
	dep := createDep(t, s, gpuDigest)
	dep = waitDep(t, s, dep.ID, "healthy")

	// A third node arrives and is approved.
	tok3 := s.IssueToken(t)
	a3 := StartAgent(t, s, AgentOpts{Hostname: "spark-3", Token: tok3, IP: "10.0.0.13/24"})
	n3 := a3.NodeID()
	s.ApproveNode(t, n3)
	s.WaitOnline(t, n3)

	nodes, err := s.Q.ListNodes(s.Ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(nodes))
	}

	// No regeneration: the new node hosts a rank of a different installed
	// recipe (CPU two-rank; node2's GPU stays leased to the GPU deployment,
	// so the CPU rank on node2 proves per-resource, not per-node, leasing).
	cpuDigest := install(t, s, FixtureRecipe{Name: "cpu-2r", Version: "1.0.0", NodeCount: 2})
	dep2 := createDep(t, s, cpuDigest,
		deploy.PlacementOverride{Rank: 0, NodeID: n2},
		deploy.PlacementOverride{Rank: 1, NodeID: n3},
	)
	dep2 = waitDep(t, s, dep2.ID, "healthy")

	cs := containersOf(a3.RT, dep2)
	if c, ok := cs[1]; !ok || c == nil || c.State != "running" {
		t.Fatalf("rank 1 not running on the new node (have %v)", a3.RT.Containers())
	}
	for _, pl := range dep2.Placements {
		if pl.Rank == 1 && pl.NodeID != n3 {
			t.Errorf("rank 1 placement = %s, want %s", pl.NodeID, n3)
		}
	}
	// The original deployment was not disturbed.
	if st := depState(s, dep.ID); st != "healthy" {
		t.Errorf("gpu deployment state = %s, want healthy", st)
	}
}
