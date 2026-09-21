package deploy

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const upstreamClusterManifest = `{
  "apiVersion":"localmodelworks/v1alpha1","kind":"Recipe",
  "metadata":{"name":"upstream-fixture","version":"1.0.0","source":{"url":"https://fixtures.local/upstream","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","path":"."}},
  "compatibility":{"nodeCount":2},"artifacts":[],
  "workloads":[{"ranks":[0,1],"upstream":{"coordinatorRank":0,"start":["./start.sh"],"stop":["./stop.sh"],"containers":["authored-service"]},"env":{"PEER":"${cluster.node.1.address}"},"permissions":["host.upstream-exec"]}]
}`

func setUpstreamNodeInventory(t *testing.T, h *harness, nodeID, address string, enabled bool) {
	t.Helper()
	inv := inventory.Inventory{
		Hostname: nodeID, CacheRoots: []inventory.CacheRoot{{Path: "/cache", Writable: true}},
		Interfaces: []inventory.Interface{{Name: "eth0", Addresses: []string{address}}},
	}
	if enabled {
		inv.ProtocolFeatures = []string{runtime.UpstreamProtocolFeature}
	}
	encoded, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: nodeID, Inventory: nullString(string(encoded))}); err != nil {
		t.Fatal(err)
	}
}

func newUpstreamHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.seedNode(t, "head", nil, "")
	h.seedNode(t, "worker", nil, "")
	setUpstreamNodeInventory(t, h, "head", "192.0.2.1", true)
	setUpstreamNodeInventory(t, h, "worker", "192.0.2.2", true)
	h.seedRecipe(t, "recipe-upstream", upstreamClusterManifest)
	return h
}

func upstreamPlacements() []PlacementOverride {
	return []PlacementOverride{{NodeID: "head", Rank: 0}, {NodeID: "worker", Rank: 1}}
}

func TestUpstreamConfigurationRequiresCapableAgentsOnlyWhenApplied(t *testing.T) {
	h := newUpstreamHarness(t)
	ctx := context.Background()
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Parameters = []recipe.Parameter{{Name: "context_length", Type: "int", Optional: true}}
	manifest.Workloads[0].Upstream.Configuration = []sourceconfig.File{{
		Path: "start.sh", SHA256: strings.Repeat("1", 64),
		Edits: []sourceconfig.Edit{{Start: 0, End: 1, Parameter: "context_length", Format: "shell"}},
	}}
	document, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	h.seedRecipe(t, "recipe-configurable", string(document))
	request := PlanRequest{RecipeDigest: "recipe-configurable", Placements: upstreamPlacements()}
	untouched, err := h.svc.Plan(ctx, request)
	if err != nil || !untouched.Ready {
		t.Fatalf("omitted optional adaptation should preserve the original launch: %+v, %v", untouched, err)
	}
	request.Parameters = map[string]any{"context_length": 8192}
	blocked, err := h.svc.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Ready {
		t.Fatal("old agents could silently ignore runtime configuration")
	}
	found := false
	for _, diagnostic := range blocked.Diagnostics {
		found = found || diagnostic.Code == "upstream.configuration_unavailable"
	}
	if !found {
		t.Fatalf("missing actionable agent capability diagnostic: %+v", blocked.Diagnostics)
	}
	for _, nodeID := range []string{"head", "worker"} {
		node, err := h.q.GetNode(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		var inv inventory.Inventory
		if err := json.Unmarshal([]byte(node.Inventory.String), &inv); err != nil {
			t.Fatal(err)
		}
		inv.ProtocolFeatures = append(inv.ProtocolFeatures, runtime.UpstreamConfigurationProtocolFeature)
		data, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: nodeID, Inventory: nullString(string(data))}); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := h.svc.Plan(ctx, request)
	if err != nil || !ready.Ready {
		t.Fatalf("capable agents cannot apply the reviewed configuration: %+v, %v", ready, err)
	}
}

func TestUpstreamCoordinatorWaitsForArmedObserverAndObservedHealth(t *testing.T) {
	h := newUpstreamHarness(t)
	ctx := context.Background()
	deployment := h.createDeployment(t, "recipe-upstream", upstreamPlacements()...)
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
			h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
		}
	}
	var observerStart *agentv1.WorkloadCommand
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_START {
			continue
		}
		if sent.nodeID == "head" {
			t.Fatal("coordinator started before observer armed")
		}
		observerStart = command
	}
	if observerStart == nil {
		t.Fatal("observer was not armed")
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: observerStart.GetCommandId(), Ok: true})
	if got := runState(t, h, deployment.RunID); got != string(runs.Waiting) {
		t.Fatalf("observer worker spawn marked serving run %s", got)
	}
	for _, sent := range h.nodes.workloadCommands() {
		if sent.nodeID == "head" && sent.msg.GetWorkloadCommand().GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
			t.Fatal("coordinator started before observer setup completed")
		}
	}
	h.svc.OnStateUpdate(ctx, "worker", &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: 1, State: "observing"})
	var headStart *agentv1.WorkloadCommand
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if sent.nodeID == "head" && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
			headStart = command
		}
	}
	if headStart == nil {
		t.Fatal("armed observer did not wake coordinator")
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: headStart.GetCommandId(), Ok: true})
	h.svc.OnStateUpdate(ctx, "head", &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: 0, State: "running"})
	if got := runState(t, h, deployment.RunID); got != string(runs.Waiting) {
		t.Fatalf("serving run became %s before worker container observation", got)
	}
	h.svc.OnStateUpdate(ctx, "worker", &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: 1, State: "running"})
	if got := runState(t, h, deployment.RunID); got != string(runs.Running) {
		t.Fatalf("independently observed service remains %s", got)
	}
	if row := deploymentRow(t, h, deployment.ID); row.ObservedState != "healthy" {
		t.Fatalf("observed service state = %s", row.ObservedState)
	}
}

func TestUpstreamRestartRetainsReviewedPeerInputs(t *testing.T) {
	h := newUpstreamHarness(t)
	ctx := context.Background()
	deployment := h.createDeployment(t, "recipe-upstream", upstreamPlacements()...)
	if _, err := h.svc.Stop(ctx, deployment.ID); err != nil {
		t.Fatal(err)
	}
	for rank, node := range []string{"head", "worker"} {
		h.svc.OnStateUpdate(ctx, node, &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: int32(rank), State: "missing"})
	}
	setUpstreamNodeInventory(t, h, "worker", "198.51.100.2", true)
	restarted, err := h.svc.Start(ctx, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitAcquisition(t, h.svc)
	placement := ParsePlacementSet(deploymentRow(t, h, deployment.ID).Placement).EntryFor(0)
	spec, err := h.svc.renderSpec(ctx, deployment.ID, 0, restarted.RunID, placement)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Env, "PEER=192.0.2.2") || slices.Contains(spec.Env, "PEER=198.51.100.2") {
		t.Fatalf("restart changed approved host execution inputs: %v", spec.Env)
	}
	if spec.Upstream == nil || spec.Upstream.Revision != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("restart lost reviewed source identity")
	}
}

func TestUpstreamPlanningRequiresAuthorityAndExclusiveNode(t *testing.T) {
	h := newUpstreamHarness(t)
	ctx := context.Background()
	setUpstreamNodeInventory(t, h, "worker", "192.0.2.2", false)
	plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "recipe-upstream", Placements: upstreamPlacements()})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready {
		t.Fatal("source execution accepted without observer node authority")
	}
	setUpstreamNodeInventory(t, h, "worker", "192.0.2.2", true)
	h.createDeployment(t, "recipe-upstream", upstreamPlacements()...)
	h.seedRecipe(t, "recipe-native", noArtifactManifest)
	plan, err = h.svc.Plan(ctx, PlanRequest{RecipeDigest: "recipe-native", Placements: []PlacementOverride{{NodeID: "head", Rank: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready {
		t.Fatal("managed launch shared a node owned by unrestricted upstream execution")
	}
}

func TestUpstreamConfigurationBindsReviewedAccelerators(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	const selected = "GPU-6ac06ab6-ac30-f3ad-b246-064a8e6f1309"
	const other = "GPU-e665a1dd-c279-05ca-a396-e7b95d783790"
	const original = "docker run --gpus all image python -m sglang.launch_server"
	for _, tc := range []struct {
		name         string
		accelerator  string
		accelerators []string
		want         string
	}{
		{name: "selected UUID", accelerator: selected, want: selected},
		{name: "ordered group", accelerator: other, accelerators: []string{selected, other}, want: selected + "," + other},
		{name: "single group member", accelerators: []string{selected}, want: selected},
		{name: "missing UUID"},
		{name: "incomplete group", accelerator: selected, accelerators: []string{selected, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
			if err != nil {
				t.Fatal(err)
			}
			workload := &manifest.Workloads[0]
			workload.Upstream.CoordinatorRank = nil
			start := strings.Index(original, "image")
			workload.Upstream.Configuration = []sourceconfig.File{{
				Path: "start.sh", SHA256: strings.Repeat("a", 64),
				Edits: []sourceconfig.Edit{{Start: start, End: start, Template: `["--env=CUDA_VISIBLE_DEVICES=${node.accelerators}"]`, Format: "argv"}},
			}}
			plan := &Plan{
				Placements: []Placement{{NodeID: "head", Rank: 0, AcceleratorUUID: tc.accelerator, Accelerators: tc.accelerators}},
				Parameters: map[string]any{"node.accelerators": other, "CUDA_VISIBLE_DEVICES": other},
			}
			service := &Service{}
			err = service.previewUpstream(context.Background(), plan, manifest, workload, map[int]recipe.RenderNode{0: {NodeID: "head"}, 1: {NodeID: "worker", NodeAddress: "198.51.100.2"}})
			if tc.want == "" {
				if err == nil || len(plan.Upstream) != 0 {
					t.Fatal("source launch accepted without a complete reviewed accelerator binding")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"run", "--gpus", "all", "--env=CUDA_VISIBLE_DEVICES=" + tc.want, "image", "python", "-m", "sglang.launch_server"}
			assertConfigured := func(files []sourceconfig.ResolvedFile) {
				t.Helper()
				if len(files) != 1 {
					t.Fatalf("missing reviewed configuration: %+v", files)
				}
				script, err := sourceconfig.Apply([]byte(original), files[0].Edits)
				if err != nil {
					t.Fatal(err)
				}
				output, err := exec.Command("bash", "-c", "docker() { printf '%s\\0' \"$@\"; }\n"+string(script)).CombinedOutput()
				got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("configured Docker arguments = %q, want %q: %v", got, want, err)
				}
			}
			assertConfigured(plan.Upstream[0].Configuration)
			digest := "sha256:" + strings.Repeat("b", 64)
			persisted, err := json.Marshal(placementSet{
				Entries: plan.Placements, Upstream: plan.Upstream,
				AcquisitionResources: []downloads.Resource{{
					NodeID: "head", Required: true,
					ResourceSpec: downloads.ResourceSpec{
						Kind: downloads.ResourceRecipe, Identity: "recipe://" + digest,
						Source:      downloads.SourceSpec{Type: downloads.SourceRecipe, Digest: digest},
						Destination: "/var/lib/lmw/recipes/" + strings.TrimPrefix(digest, "sha256:"),
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			// Dispatch must use the reviewed bytes, not changed placement or settings.
			plan.Placements[0].AcceleratorUUID = other
			plan.Placements[0].Accelerators = []string{other}
			plan.Parameters["node.accelerators"] = "changed-after-review"
			workload.Upstream.Configuration[0].Edits[0].Template = `["--env=CUDA_VISIBLE_DEVICES=changed-after-review"]`
			spec, err := frozenUpstreamSpec(db.GetDeploymentRow{ID: "deployment", RecipeDigest: digest, Placement: string(persisted)}, &plan.Placements[0], "run")
			if err != nil {
				t.Fatal(err)
			}
			assertConfigured(spec.Upstream.Configuration)
		})
	}
}
