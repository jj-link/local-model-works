package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	fabriccfg "github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipe/repositorycompiler"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// dsgb10Sparks lists the fleet in placement order: spark4 is the head (rank 0).
var dsgb10Sparks = []struct{ node, address, link string }{
	{"spark4", "100.64.0.4", "10.10.0.14"},
	{"spark1", "100.64.0.1", "10.10.0.11"},
	{"spark2", "100.64.0.2", "10.10.0.12"},
	{"spark3", "100.64.0.3", "10.10.0.13"},
}

func compileDSGB10(t *testing.T) []byte {
	t.Helper()
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	id, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: repositorycompiler.DSGB10RepositoryURL, Path: repositorycompiler.DSGB10SourcePath})
	if err != nil {
		t.Fatal(err)
	}
	source := recipe.RepositorySource{URL: repositorycompiler.DSGB10RepositoryURL, Path: repositorycompiler.DSGB10SourcePath, CommitSHA: "f2117916f2a4cac0f79fa41680ebd39bf5d42b51", RepositoryID: id}
	compiler, ok := repositorycompiler.NewRegistry(validator).LookupUpstream(source)
	if !ok {
		t.Fatal("DSGB10 procedure is not registered")
	}
	packed, err := compiler.CompileRetained(context.Background(), source, func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join("..", "recipe", "repositorycompiler", "testdata", "deepseek-v41-flash-dsgb10-spark-tp4", name))
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return packed.ConfigJSON
}

func newDSGB10Harness(t *testing.T, features ...string) *harness {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	var members []string
	var bindings []fabriccfg.MemberBinding
	gid := int32(3)
	for _, spark := range dsgb10Sparks {
		h.seedNode(t, spark.node, nil, "")
		inv := inventory.Inventory{
			Hostname: spark.node, CacheRoots: []inventory.CacheRoot{{Path: "/cache", Writable: true}},
			Accelerators:     []inventory.Accelerator{{Index: 0, Vendor: "nvidia", Architecture: "sm_121", Name: "GB10", UUID: "GPU-" + spark.node, MemoryBytes: 1 << 30}},
			Interfaces:       []inventory.Interface{{Name: "tailscale0", Addresses: []string{spark.address}}},
			ProtocolFeatures: features,
		}
		encoded, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: spark.node, Inventory: nullString(string(encoded))}); err != nil {
			t.Fatal(err)
		}
		members = append(members, spark.node)
		bindings = append(bindings, fabriccfg.MemberBinding{NodeID: spark.node, InterfaceName: "enp1s0f0np0", Address: spark.link, RDMADevice: "rocep1s0f0", GIDIndex: &gid})
	}
	memberJSON, _ := json.Marshal(members)
	bindingJSON, _ := json.Marshal(bindings)
	if err := h.q.CreateFabric(ctx, db.CreateFabricParams{ID: "fabric-switch", Name: "spark-switch", Transport: fabriccfg.TransportRoCE, Members: string(memberJSON), Bindings: string(bindingJSON), Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.q.UpdateFabricState(ctx, db.UpdateFabricStateParams{State: "ok", Diagnostics: "[]", ID: "fabric-switch"}); err != nil {
		t.Fatal(err)
	}
	h.seedRecipe(t, "recipe-dsgb10", string(compileDSGB10(t)))
	return h
}

func dsgb10Request() PlanRequest {
	request := PlanRequest{RecipeDigest: "recipe-dsgb10", Parameters: map[string]any{
		"fleet_file": "/home/jjlink/fleet.conf",
		"tokenizer":  "/home/jjlink/models/DeepSeek-V4.1-Flash-1017/tokenizer.json",
		"cf_mlp":     "/home/jjlink/cfmlp_cml.bin",
	}}
	for rank, spark := range dsgb10Sparks {
		request.Placements = append(request.Placements, PlacementOverride{NodeID: spark.node, Rank: int32(rank)})
	}
	return request
}

func TestDSGB10FourSparkPlanAndHealth(t *testing.T) {
	ctx := context.Background()
	old := newDSGB10Harness(t, runtime.UpstreamProtocolFeature, runtime.UpstreamConfigurationProtocolFeature)
	blocked, err := old.svc.Plan(ctx, dsgb10Request())
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Ready || !slices.ContainsFunc(blocked.Diagnostics, func(d diag.Diagnostic) bool { return d.Code == "upstream.status_unavailable" }) {
		t.Fatalf("agents without status observation accepted: %+v", blocked.Diagnostics)
	}

	h := newDSGB10Harness(t, runtime.UpstreamProtocolFeature, runtime.UpstreamConfigurationProtocolFeature, runtime.UpstreamStatusProtocolFeature)
	plan, err := h.svc.Plan(ctx, dsgb10Request())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready {
		t.Fatalf("four-Spark plan not ready: %+v %+v", plan.Diagnostics, plan.Conflicts)
	}
	if plan.Fabric == nil || *plan.Fabric != "fabric-switch" {
		t.Fatalf("switched four-member fabric not selected: %v", plan.Fabric)
	}
	if plan.Endpoint.Host != "100.64.0.4" || plan.Endpoint.Port != 8888 || plan.Endpoint.Path != "/health" {
		t.Fatalf("endpoint must be the head's port 8888: %+v", plan.Endpoint)
	}
	if len(plan.Upstream) != 4 {
		t.Fatalf("upstream previews = %d", len(plan.Upstream))
	}
	for _, preview := range plan.Upstream {
		execution := preview.Execution
		if preview.Rank == 0 {
			if preview.NodeID != "spark4" || len(execution.Status) != 2 || len(execution.Install) != 3 || len(preview.Configuration) != 1 ||
				preview.Environment["DSGB10_ADDR"] != "127.0.0.1 100.64.0.4" || preview.Environment["FLEET"] != "/home/jjlink/fleet.conf" ||
				preview.Environment["DSGB10_PORT"] != "8888" || preview.Environment["DSGB10_PROD"] != "1" {
				t.Fatalf("coordinator preview: %+v", preview)
			}
			continue
		}
		if len(execution.Status) != 0 || len(execution.Install) != 0 || len(execution.Containers) != 0 || len(preview.Configuration) != 0 {
			t.Fatalf("rank %d must only hold placement: %+v", preview.Rank, preview)
		}
	}

	request := dsgb10Request()
	deployment, err := h.createReviewed(ctx, CreateRequest{RecipeDigest: request.RecipeDigest, Placements: request.Placements, Parameters: request.Parameters})
	if err != nil {
		t.Fatal(err)
	}
	waitAcquisition(t, h.svc)
	h.svc.readinessProbe = func(_ context.Context, row db.GetDeploymentRow) (bool, string) {
		return row.Endpoint.String == "100.64.0.4:8888", "waiting for the head endpoint"
	}
	specs := map[string]runtime.ContainerSpec{}
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
			continue
		}
		var spec runtime.ContainerSpec
		if err := json.Unmarshal(command.GetContainerSpec(), &spec); err != nil {
			t.Fatal(err)
		}
		specs[sent.nodeID] = spec
		h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
	}
	head := specs["spark4"].Upstream
	if head == nil || head.ObserveOnly || len(head.Status) == 0 || len(head.Containers) != 0 || len(head.Configuration) != 1 {
		t.Fatalf("head spec: %+v", head)
	}
	for _, spark := range dsgb10Sparks[1:] {
		holder := specs[spark.node].Upstream
		if holder == nil || !holder.ObserveOnly || len(holder.Status) != 0 || len(holder.Install) != 0 || len(holder.Configuration) != 0 || len(holder.Containers) != 0 {
			t.Fatalf("%s spec must be a placement holder: %+v", spark.node, holder)
		}
	}
	startHead := func() *agentv1.WorkloadCommand {
		for _, sent := range h.nodes.workloadCommands() {
			if sent.nodeID == "spark4" && sent.msg.GetWorkloadCommand().GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
				return sent.msg.GetWorkloadCommand()
			}
		}
		return nil
	}
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START && sent.nodeID != "spark4" {
			h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
		}
	}
	if startHead() != nil {
		t.Fatal("head started before the placement holders were armed")
	}
	for rank, spark := range dsgb10Sparks[1:] {
		h.svc.OnStateUpdate(ctx, spark.node, &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: int32(rank + 1), State: "running"})
	}
	command := startHead()
	if command == nil {
		t.Fatal("armed placement holders did not wake the head")
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
	h.svc.OnStateUpdate(ctx, "spark4", &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: 0, State: "running"})
	row := deploymentRow(t, h, deployment.ID)
	if row.ObservedState != "healthy" || row.Endpoint.String != "100.64.0.4:8888" {
		t.Fatalf("status-observed head with placement holders: observed %s at %s", row.ObservedState, row.Endpoint.String)
	}
}
