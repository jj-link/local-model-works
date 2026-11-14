package deploy

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const nodePortManifest = `{
  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"node-port","version":"1"},
  "workloads":[{
    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
    "command":["serve"],"args":["--public-port","${node.port}"],
    "env":{"PUBLIC_PORT":"${node.port}"},
    "resources":{"pids":64},
    "ports":[{"container":8000,"host":8000},{"container":9100}],
    "readiness":{"httpGet":{"path":"/health","port":8000}}
  }]
}`

func upstreamSingleManifest(port string) string {
	return `{
  "apiVersion":"localmodelworks/v1alpha1","kind":"Recipe",
  "metadata":{"name":"upstream-single","version":"1.0.0","source":{"url":"https://fixtures.local/single","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","path":"."}},
  "compatibility":{"nodeCount":1},"artifacts":[],
  "workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["authored-service"]},
    "env":{"PORT":"` + port + `"},"permissions":["host.upstream-exec"],
    "ports":[{"container":8000,"host":8000}],"readiness":{"httpGet":{"path":"/health","port":8000}}}]
}`
}

func setNodeLabels(t *testing.T, h *harness, nodeID, labels string) {
	t.Helper()
	if err := h.q.UpdateNodeMeta(context.Background(), db.UpdateNodeMetaParams{DisplayName: nodeID, Labels: labels, ID: nodeID}); err != nil {
		t.Fatal(err)
	}
}

func createdSpec(t *testing.T, h *harness, deploymentID string) runtime.ContainerSpec {
	t.Helper()
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if command.GetDeploymentId() != deploymentID || command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
			continue
		}
		var spec runtime.ContainerSpec
		if err := json.Unmarshal(command.GetContainerSpec(), &spec); err != nil {
			t.Fatal(err)
		}
		return spec
	}
	t.Fatalf("no create command for deployment %s", deploymentID)
	return runtime.ContainerSpec{}
}

func TestServePortLabelPinsSingleNodeDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "labelled", nil, "")
	h.seedNode(t, "plain", nil, "")
	h.seedNode(t, "invalid", nil, "")
	setNodeLabels(t, h, "labelled", `{"serve-port":"8003","rack":"a"}`)
	setNodeLabels(t, h, "invalid", `{"serve-port":"80000"}`)
	h.seedRecipe(t, "recipe-node-port", nodePortManifest)

	for nodeID, want := range map[string]int32{"labelled": 8003, "plain": 8000, "invalid": 8000} {
		plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "recipe-node-port", Placements: []PlacementOverride{{NodeID: nodeID, Rank: 0}}})
		if err != nil || !plan.Ready {
			t.Fatalf("%s plan: %+v, %v", nodeID, plan, err)
		}
		if plan.Placements[0].Port != want || plan.Endpoint.Port != want {
			t.Fatalf("%s serving port = placement %d endpoint %d, want %d", nodeID, plan.Placements[0].Port, plan.Endpoint.Port, want)
		}
		wantPorts := []PortPreview{
			{NodeID: nodeID, NodeName: nodeID, HostPort: want, ContainerPort: 8000, Protocol: "tcp"},
			{NodeID: nodeID, NodeName: nodeID, HostPort: 9100, ContainerPort: 9100, Protocol: "tcp"},
		}
		if !slices.Equal(plan.Ports, wantPorts) {
			t.Fatalf("%s published ports = %+v", nodeID, plan.Ports)
		}
		if leases := plan.LeaseResources(); !slices.Contains(leases, "port:"+nodeID+":"+strconv.Itoa(int(want))) || !slices.Contains(leases, "port:"+nodeID+":9100") {
			t.Fatalf("%s leases = %v", nodeID, leases)
		}
	}

	dep := h.createDeployment(t, "recipe-node-port", PlacementOverride{NodeID: "labelled", Rank: 0})
	spec := createdSpec(t, h, dep.ID)
	if len(spec.Ports) != 2 || spec.Ports[0] != (runtime.PortSpec{Host: 8003, Container: 8000, Protocol: "tcp"}) || spec.Ports[1].Host != 9100 {
		t.Fatalf("native publish = %+v, want host 8003 -> container 8000", spec.Ports)
	}
	if !slices.Equal(spec.Cmd, []string{"--public-port", "8003"}) || !slices.Contains(spec.Env, "PUBLIC_PORT=8003") {
		t.Fatalf("${node.port} rendering = cmd %v env %v", spec.Cmd, spec.Env)
	}
	row := deploymentRow(t, h, dep.ID)
	if !strings.HasSuffix(row.Endpoint.String, ":8003") || ParsePlacementSet(row.Placement).EntryFor(0).Port != 8003 {
		t.Fatalf("persisted endpoint %q placement %s", row.Endpoint.String, row.Placement)
	}
	manifest, err := recipe.Parse([]byte(nodePortManifest))
	if err != nil {
		t.Fatal(err)
	}
	workload := &manifest.Workloads[0]
	if got := probeAddress(row.Endpoint.String, 8000, workload, row.Placement); got != "100.86.3.45:8003" {
		t.Fatalf("serving-port probe = %s, want the frozen port", got)
	}
	if got := probeAddress(row.Endpoint.String, 9100, workload, row.Placement); got != "100.86.3.45:9100" {
		t.Fatalf("auxiliary probe = %s, want its declared port", got)
	}
	if got := probeAddress(row.Endpoint.String, 8000, workload, `{"ranks":{"labelled":0}}`); got != "100.86.3.45:8000" {
		t.Fatalf("legacy row probe = %s, want the declared port", got)
	}
	// Endpoint recovery after an agent lost its spec keeps the frozen port.
	if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET endpoint = NULL WHERE id = ?", dep.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.OnStateUpdate(ctx, "labelled", &agentv1.StateUpdate{DeploymentId: dep.ID, RunId: dep.RunID, Rank: 0, ContainerId: "c", State: "running"})
	if got := deploymentRow(t, h, dep.ID).Endpoint.String; got != "100.86.3.45:8003" {
		t.Fatalf("recovered endpoint = %q", got)
	}
}

func TestServePortLabelIgnoredByMultiNodeWorkloads(t *testing.T) {
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"cluster-port","version":"1"},
	  "compatibility":{"nodeCount":2},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":[],"ranks":[0,1],"resources":{"pids":64},
	    "ports":[{"container":8888}]
	  }]
	}`
	h := newHarness(t)
	h.seedNode(t, "head", nil, "")
	h.seedNode(t, "worker", nil, "")
	setNodeLabels(t, h, "head", `{"serve-port":"8001"}`)
	setNodeLabels(t, h, "worker", `{"serve-port":"8002"}`)
	h.seedRecipe(t, "recipe-cluster-port", manifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "recipe-cluster-port", Placements: []PlacementOverride{{NodeID: "head", Rank: 0}, {NodeID: "worker", Rank: 1}}})
	if err != nil || !plan.Ready {
		t.Fatalf("plan: %+v, %v", plan, err)
	}
	if plan.Placements[0].Port != 8888 || plan.Placements[1].Port != 8889 || plan.Endpoint.Port != 8888 {
		t.Fatalf("cluster ports = %+v endpoint %d, want the recipe rule", plan.Placements, plan.Endpoint.Port)
	}
}

func TestRestartKeepsFrozenServePort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "spark", nil, "")
	setNodeLabels(t, h, "spark", `{"serve-port":"8003"}`)
	h.seedRecipe(t, "recipe-node-port", nodePortManifest)
	dep := h.createDeployment(t, "recipe-node-port", PlacementOverride{NodeID: "spark", Rank: 0})
	crash := func(runID string) {
		t.Helper()
		if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET dispatch=?, observed_state='healthy' WHERE id=?", `{"0":"started"}`, dep.ID); err != nil {
			t.Fatal(err)
		}
		h.svc.OnStateUpdate(ctx, "spark", &agentv1.StateUpdate{DeploymentId: dep.ID, RunId: runID, ContainerId: "c", State: "exited", Rank: 0, ExitCode: 1})
	}
	restart := func(wantPort string) string {
		t.Helper()
		started, err := h.svc.Start(ctx, dep.ID)
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		row := deploymentRow(t, h, dep.ID)
		if !strings.HasSuffix(row.Endpoint.String, ":"+wantPort) {
			t.Fatalf("restarted endpoint = %q, want port %s", row.Endpoint.String, wantPort)
		}
		var active int
		if err := h.dbh.QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE resource=? AND state='active' AND owner_id=?", "port:spark:"+wantPort, dep.ID).Scan(&active); err != nil || active != 1 {
			t.Fatalf("port lease %s active=%d err=%v", wantPort, active, err)
		}
		return started.RunID
	}
	crash(dep.RunID)
	setNodeLabels(t, h, "spark", `{"serve-port":"8004"}`)
	runID := restart("8003")
	if port := ParsePlacementSet(deploymentRow(t, h, dep.ID).Placement).EntryFor(0).Port; port != 8003 {
		t.Fatalf("restart re-froze port %d", port)
	}

	// Rows planned before ports were frozen keep the recipe port, not the label.
	crash(runID)
	placement := ParsePlacementSet(deploymentRow(t, h, dep.ID).Placement)
	placement.Entries[0].Port = 0
	if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET placement=? WHERE id=?", placement.Marshal(), dep.ID); err != nil {
		t.Fatal(err)
	}
	restart("8000")
}

func TestUpstreamSingleNodeBindsLabelledPort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "spark", nil, "")
	setUpstreamNodeInventory(t, h, "spark", "192.0.2.5", true)
	setNodeLabels(t, h, "spark", `{"serve-port":"8002"}`)
	h.seedRecipe(t, "recipe-bound", upstreamSingleManifest("${node.port}"))
	h.seedRecipe(t, "recipe-literal", upstreamSingleManifest("8000"))
	request := PlanRequest{RecipeDigest: "recipe-literal", Placements: []PlacementOverride{{NodeID: "spark", Rank: 0}}}
	literal, err := h.svc.Plan(ctx, request)
	if err != nil || !literal.Ready {
		t.Fatalf("literal plan: %+v, %v", literal, err)
	}
	if literal.Endpoint.Port != 8000 || literal.Upstream[0].Environment["PORT"] != "8000" {
		t.Fatal("a procedure that cannot move its listener must keep its declared port")
	}
	request.RecipeDigest = "recipe-bound"
	plan, err := h.svc.Plan(ctx, request)
	if err != nil || !plan.Ready {
		t.Fatalf("bound plan: %+v, %v", plan, err)
	}
	if plan.Endpoint.Port != 8002 || plan.Ports[0].HostPort != 8002 || plan.Upstream[0].Environment["PORT"] != "8002" {
		t.Fatalf("upstream port = endpoint %d ports %+v env %v", plan.Endpoint.Port, plan.Ports, plan.Upstream[0].Environment)
	}
	dep := h.createDeployment(t, "recipe-bound", PlacementOverride{NodeID: "spark", Rank: 0})
	if spec := createdSpec(t, h, dep.ID); !slices.Contains(spec.Env, "PORT=8002") {
		t.Fatalf("upstream launch env = %v", spec.Env)
	}

	target, err := recipe.Parse([]byte(upstreamSingleManifest("${node.port}")))
	if err != nil {
		t.Fatal(err)
	}
	target.Metadata.Source.Revision = strings.Repeat("b", 40)
	document, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	h.seedRecipe(t, digest, string(document))
	updatedPort := func() string {
		t.Helper()
		inputs, err := h.svc.installationUpdateSpecs(ctx, "spark", digest)
		if err != nil || len(inputs) != 1 {
			t.Fatalf("installation update specs = %+v, %v", inputs, err)
		}
		var spec runtime.ContainerSpec
		if err := json.Unmarshal(inputs[0].Spec, &spec); err != nil {
			t.Fatal(err)
		}
		for _, entry := range spec.Env {
			if port, ok := strings.CutPrefix(entry, "PORT="); ok {
				return port
			}
		}
		t.Fatalf("update spec has no PORT: %v", spec.Env)
		return ""
	}
	setNodeLabels(t, h, "spark", `{"serve-port":"8004"}`)
	if port := updatedPort(); port != "8002" {
		t.Fatalf("recipe update moved the frozen port to %s", port)
	}
	// An installation planned before ports were frozen keeps its recipe port.
	if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET recipe_digest = 'recipe-literal' WHERE id = ?", dep.ID); err != nil {
		t.Fatal(err)
	}
	placement := ParsePlacementSet(deploymentRow(t, h, dep.ID).Placement)
	placement.Entries[0].Port = 0
	placement.Upstream[0].Environment["PORT"] = "8000"
	if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET placement = ? WHERE id = ?", placement.Marshal(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if port := updatedPort(); port != "8000" {
		t.Fatalf("legacy installation update bound port %s, want 8000", port)
	}
}
