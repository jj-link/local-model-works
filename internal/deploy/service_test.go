package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
	fabriccfg "github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// ---------------------------------------------------------------- fixtures

// fakeNodes is an in-memory NodeSender that records every outbound message.
type fakeNodes struct {
	mu     sync.Mutex
	online map[string]bool
	msgs   []sentMsg
	onSend func(*agentv1.ServerMessage)
	settle func()
}

type sentMsg struct {
	nodeID string
	msg    *agentv1.ServerMessage
}

func (f *fakeNodes) Send(nodeID string, m *agentv1.ServerMessage) bool {
	f.mu.Lock()
	if !f.online[nodeID] {
		f.mu.Unlock()
		return false
	}
	f.msgs = append(f.msgs, sentMsg{nodeID: nodeID, msg: m})
	onSend := f.onSend
	f.mu.Unlock()
	if onSend != nil {
		onSend(m)
	}
	return true
}

func (f *fakeNodes) Online(nodeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[nodeID]
}

func (f *fakeNodes) setOnline(nodeID string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online[nodeID] = on
}

func (f *fakeNodes) transferCommands() []sentMsg {
	if f.settle != nil {
		f.settle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMsg
	for _, s := range f.msgs {
		if s.msg.GetTransferCommand() != nil {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeNodes) workloadCommands() []sentMsg {
	if f.settle != nil {
		f.settle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMsg
	for _, s := range f.msgs {
		if s.msg.GetWorkloadCommand() != nil {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeNodes) artifactCommands() []sentMsg {
	if f.settle != nil {
		f.settle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMsg
	for _, sent := range f.msgs {
		if sent.msg.GetArtifactCommand() != nil {
			out = append(out, sent)
		}
	}
	return out
}

func (f *fakeNodes) extensionCommands() []sentMsg {
	if f.settle != nil {
		f.settle()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMsg
	for _, sent := range f.msgs {
		if sent.msg.GetExtensionCommand() != nil {
			out = append(out, sent)
		}
	}
	return out
}

// harness wires a Service against a fresh migrated SQLite database.
type harness struct {
	svc   *Service
	nodes *fakeNodes
	q     *db.Queries
	dbh   *sql.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := db.New(sqlDB)
	bus := events.NewEventBus(q)
	nodes := &fakeNodes{online: map[string]bool{}}
	svc := New(sqlDB, q, bus, runs.New(sqlDB, q, bus, t.TempDir()), nodes)
	svc.downloads = &fakeAcquisition{service: svc}
	nodes.settle = func() { waitAcquisition(t, svc) }
	t.Cleanup(nodes.settle)
	return &harness{svc: svc, nodes: nodes, q: q, dbh: sqlDB}
}

func inventoryWith(accs []inventory.Accelerator, peerListen string) string {
	b, _ := json.Marshal(inventory.Inventory{
		CacheRoots:   []inventory.CacheRoot{{Path: "/cache", Writable: true}},
		Hostname:     "testhost",
		Accelerators: accs,
		Interfaces: []inventory.Interface{
			{Name: "enx0", Addresses: []string{"100.86.3.45"}},
		},
		PeerListen: peerListen,
	})
	return string(b)
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func (h *harness) seedNode(t *testing.T, nodeID string, accs []inventory.Accelerator, peerListen string) {
	t.Helper()
	ctx := context.Background()
	if err := h.q.CreateNode(ctx, db.CreateNodeParams{ID: nodeID, DisplayName: nodeID, Labels: "{}"}); err != nil {
		t.Fatalf("create node %s: %v", nodeID, err)
	}
	if err := h.q.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "online", ID: nodeID}); err != nil {
		t.Fatalf("status %s: %v", nodeID, err)
	}
	if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{
		ID: nodeID, Inventory: nullString(inventoryWith(accs, peerListen)),
	}); err != nil {
		t.Fatalf("inventory %s: %v", nodeID, err)
	}
	h.nodes.setOnline(nodeID, true)
}

func (h *harness) seedHostTelemetry(t *testing.T, nodeID string, swapTotal uint64, swappiness uint32) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"memory":{"used_bytes":34359738368,"total_bytes":137438953472,"swap_total_bytes":%d,"swappiness":%d}}`,
		swapTotal,
		swappiness,
	)
	if err := h.q.InsertTelemetry5s(context.Background(), db.InsertTelemetry5sParams{
		NodeID: nodeID, Ts: time.Now().Unix(), Payload: payload,
	}); err != nil {
		t.Fatalf("seed telemetry %s: %v", nodeID, err)
	}
}

func (h *harness) seedRecipe(t *testing.T, digest, manifest string) {
	t.Helper()
	h.seedRecipeUnplaced(t, digest, manifest)
	ctx := context.Background()
	packageID := "package-" + strings.TrimPrefix(digest, "sha256:")
	nodes, err := h.q.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if err := h.q.UpsertPlacement(ctx, db.UpsertPlacementParams{
			ArtifactID: packageID, NodeID: node.ID, Path: "/var/lib/lmw/recipes/" + digest,
			State: "valid", Diagnostics: "[]",
		}); err != nil {
			t.Fatalf("seed recipe package placement: %v", err)
		}
	}
}

func (h *harness) seedRecipeUnplaced(t *testing.T, digest, manifest string) {
	t.Helper()
	ctx := context.Background()
	if err := h.q.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: "test", Version: "1",
		Source: "{}", Manifest: manifest,
	}); err != nil {
		t.Fatalf("create recipe: %v", err)
	}
	packageID := "package-" + strings.TrimPrefix(digest, "sha256:")
	if err := h.q.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: packageID, Kind: "recipe", Identity: "recipe://" + digest,
		Digest: nullString(digest), Metadata: "{}",
	}); err != nil {
		t.Fatalf("create recipe package artifact: %v", err)
	}
}

func (h *harness) seedArtifact(t *testing.T, id, identity string) {
	t.Helper()
	ctx := context.Background()
	if err := h.q.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: id, Kind: "model", Identity: identity, Metadata: "{}",
	}); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
}

func (h *harness) seedPlacement(t *testing.T, artifactID, nodeID, path, state string) {
	t.Helper()
	ctx := context.Background()
	if err := h.q.UpsertPlacement(ctx, db.UpsertPlacementParams{
		ArtifactID: artifactID, NodeID: nodeID, Path: path, State: state,
		Diagnostics: "[]", SizeBytes: 123,
	}); err != nil {
		t.Fatalf("placement %s@%s: %v", artifactID, nodeID, err)
	}
}

const noArtifactManifest = `{
	"apiVersion": "lmw.dev/v1",
	"kind": "Recipe",
	"metadata": {"name": "test-serve", "version": "1"},
	"workloads": [{
		"image": {"reference": "test-serve:latest"},
		"command": ["serve"],
		"args": ["--port", "8000"],
		"resources": {"pids": 64},
		"ports": [{"container": 8000}],
		"readiness": {"httpGet": {"path": "/health", "port": 8000}}
	}]
}`

const gpuManifest = `{
	"apiVersion": "lmw.dev/v1",
	"kind": "Recipe",
	"metadata": {"name": "test-gpu", "version": "1"},
	"workloads": [{
		"image": {"reference": "test-serve:latest"},
		"command": ["serve"],
		"resources": {"pids": 64},
		"devices": {"accelerator": {"all": true}},
		"ports": [{"container": 8000}],
		"readiness": {"httpGet": {"path": "/health", "port": 8000}}
	}]
}`

const artifactManifest = `{
	"apiVersion": "lmw.dev/v1",
	"kind": "Recipe",
	"metadata": {"name": "test-serve", "version": "1"},
	"artifacts": [{
		"name": "model", "kind": "model",
		"source": {"type": "local", "identity": "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"mount": "/var/lib/lmw/artifacts/model"
	}],
	"workloads": [{
		"image": {"reference": "test-serve:latest"},
		"command": ["serve"],
		"args": ["--model", "{{ .Artifacts.model }}"],
		"resources": {"pids": 64},
		"ports": [{"container": 8000}],
		"readiness": {"httpGet": {"path": "/health", "port": 8000}}
	}]
}`

func TestDeploymentModelAndEngineViews(t *testing.T) {
	tests := []struct {
		name          string
		parameters    map[string]any
		manifest      string
		expectedModel string
	}{
		{
			name: "metadata fallback",
			manifest: strings.Replace(
				noArtifactManifest,
				`"metadata": {"name": "test-serve", "version": "1"}`,
				`"metadata": {"name": "test-serve", "version": "1", "model": "metadata-model", "engine": "vllm"}`,
				1,
			),
			expectedModel: "metadata-model",
		},
		{
			name:       "setting override",
			parameters: map[string]any{"model": "profile-model"},
			manifest: strings.Replace(
				strings.Replace(
					noArtifactManifest,
					`"metadata": {"name": "test-serve", "version": "1"}`,
					`"metadata": {"name": "test-serve", "version": "1", "model": "metadata-model", "engine": "vllm"}`,
					1,
				),
				`"workloads":`,
				`"parameters": [{"name": "model", "type": "string"}], "workloads":`,
				1,
			),
			expectedModel: "profile-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedNode(t, "node-1", nil, "")
			h.seedRecipe(t, "recipe-model", tt.manifest)

			plan, err := h.svc.Plan(context.Background(), PlanRequest{
				RecipeDigest: "recipe-model",
				Parameters:   tt.parameters,
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plan.Endpoint.Model != tt.expectedModel {
				t.Fatalf("plan endpoint model = %q, want %q", plan.Endpoint.Model, tt.expectedModel)
			}

			deployment, err := h.createReviewed(context.Background(), CreateRequest{
				RecipeDigest: "recipe-model",
				Parameters:   tt.parameters,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if deployment.Engine != "vllm" {
				t.Fatalf("deployment engine = %q, want vllm", deployment.Engine)
			}
			if deployment.Endpoint == nil || deployment.Endpoint.Model != tt.expectedModel {
				t.Fatalf("deployment endpoint = %+v, want model %q", deployment.Endpoint, tt.expectedModel)
			}

			view, err := h.svc.Get(context.Background(), deployment.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if view.Engine != "vllm" || view.Endpoint == nil || view.Endpoint.Model != tt.expectedModel {
				t.Fatalf("deployment view = %+v, want engine vllm and model %q", view, tt.expectedModel)
			}
		})
	}
}

const endpointManifest = `{
	"apiVersion": "lmw.dev/v1",
	"kind": "Recipe",
	"metadata": {"name": "endpoint-serve", "version": "1"},
	"parameters": [{"name": "model", "type": "string", "default": "qwen-7b"}],
	"workloads": [{
		"image": {"reference": "test-serve:latest"},
		"command": ["serve"],
		"resources": {"pids": 64},
		"devices": {"accelerator": {"all": true}},
		"ports": [{"container": 8000}],
		"readiness": {"httpGet": {"path": "/v1/health", "port": 8000}}
	}]
}`

// TestDeploymentEndpointMetadataSurvivesRead verifies the endpoint model/path
// captured at create is persisted and restored on a fresh DB read (the values
// a reopened control plane sees without re-parsing recipes).
func TestDeploymentEndpointMetadataSurvivesRead(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "100.86.3.45:4433")
	h.seedRecipe(t, "endpoint-serve", endpointManifest)
	dep, err := h.createReviewed(context.Background(), CreateRequest{RecipeDigest: "endpoint-serve", Placements: []PlacementOverride{{NodeID: "node-a", Rank: 0}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if dep.Endpoint == nil || dep.Endpoint.Model != "qwen-7b" || dep.Endpoint.Path != "/v1/health" {
		t.Fatalf("create endpoint: %+v", dep.Endpoint)
	}
	// Re-read through the store (fresh SELECT) and confirm metadata survived.
	again, err := h.svc.Get(context.Background(), dep.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Endpoint == nil || again.Endpoint.Model != "qwen-7b" || again.Endpoint.Path != "/v1/health" {
		t.Fatalf("re-read endpoint: %+v", again.Endpoint)
	}
}

func TestAdvertisedAddressRefreshesEndpointAndReadinessWithoutChangingPort(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	inv, err := inventory.Parse(inventoryWith(gpuAccs("a"), ""))
	if err != nil {
		t.Fatal(err)
	}
	inv.Interfaces = []inventory.Interface{
		{Name: "mirrored", Addresses: []string{"100.102.241.73"}},
		{Name: "tailscale0", Addresses: []string{"100.74.194.53"}},
	}
	saveInventory := func() {
		t.Helper()
		data, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: "node-a", Inventory: nullString(string(data))}); err != nil {
			t.Fatal(err)
		}
	}
	saveInventory()
	h.seedRecipe(t, "endpoint-serve", endpointManifest)
	dep := h.createDeployment(t, "endpoint-serve")
	h.svc.readinessProbe = func(_ context.Context, row db.GetDeploymentRow) (bool, string) {
		return row.Endpoint.String == "100.74.194.53:49152", "waiting for advertised address"
	}
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, Rank: 0,
		ContainerId: "running-container", State: "running", EndpointPort: 49152,
	})
	before, err := h.svc.Get(ctx, dep.ID)
	if err != nil || before.Endpoint == nil || before.Endpoint.Host != "100.102.241.73" {
		t.Fatalf("initial discovered endpoint: %+v, %v", before, err)
	}
	inv.AdvertiseAddress = "100.74.194.53"
	saveInventory()
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, Rank: 0,
		ContainerId: "running-container", State: "running",
	})
	after, err := h.svc.Get(ctx, dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Endpoint == nil || after.Endpoint.Host != "100.74.194.53" || after.Endpoint.Port != 49152 || after.ObservedState != "healthy" {
		t.Fatalf("address refresh failed to preserve the published port and recover readiness: %+v", after)
	}
	if after.RunID != dep.RunID {
		t.Fatal("address refresh restarted the model")
	}
}

// TestDeploymentLegacyEndpointFallsBackToRecipe proves a deployment whose
// persisted endpoint_model is null (a pre-migration row) still resolves its
// model from the setting/metadata fallback when the view is read back.
func TestDeploymentLegacyEndpointFallsBackToRecipe(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "100.86.3.45:4433")
	h.seedRecipe(t, "endpoint-serve", endpointManifest)
	dep, err := h.createReviewed(context.Background(), CreateRequest{RecipeDigest: "endpoint-serve", Placements: []PlacementOverride{{NodeID: "node-a", Rank: 0}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Null out the persisted endpoint identity to model a legacy row.
	if _, err := h.dbh.Exec("UPDATE deployments SET endpoint_model = NULL, endpoint_path = NULL WHERE id = ?", dep.ID); err != nil {
		t.Fatalf("null endpoint metadata: %v", err)
	}
	view, err := h.svc.Get(context.Background(), dep.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.Endpoint == nil || view.Endpoint.Model != "qwen-7b" {
		t.Fatalf("legacy view endpoint = %+v, want model fallback qwen-7b", view.Endpoint)
	}
}

func (h *harness) createDeployment(t *testing.T, digest string, overrides ...PlacementOverride) *Deployment {
	t.Helper()
	dep, err := h.createReviewed(context.Background(), CreateRequest{
		RecipeDigest: digest, Placements: overrides,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	waitAcquisition(t, h.svc)
	return dep
}

func TestRenderedSpecUsesNodeIdentityAndHardeningDefaults(t *testing.T) {
	// Historical immutable recipes may still contain removed fields. They must
	// not impose CPU or RAM restrictions when rendered by the current launcher.
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"hard","version":"1"},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":["--node","${node.id}","--addr","${node.address}"],
	    "resources":{"cpu":2,"cpusetCpus":"5-9,15-19","memoryBytes":33554432,"tmpfsBytes":67108864,"pids":128}
	  }]
	}`
	h := newHarness(t)
	h.seedNode(t, "node-exact", nil, "")
	h.seedRecipe(t, "recipe-hard", manifest)
	h.createDeployment(t, "recipe-hard", PlacementOverride{NodeID: "node-exact", Rank: 0})
	commands := h.nodes.workloadCommands()
	if len(commands) != 1 {
		t.Fatalf("workload commands = %d", len(commands))
	}
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(commands[0].msg.GetWorkloadCommand().GetContainerSpec(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.NetworkMode != "none" || !spec.ReadonlyRootfs || !spec.NoNewPrivileges ||
		len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" ||
		spec.CPU != 0 || spec.MemoryBytes != 0 || spec.PidsLimit != 128 || spec.TmpfsBytes != 67108864 {
		t.Fatalf("hardened spec = %+v", spec)
	}
	if len(spec.Cmd) < 4 || spec.Cmd[1] != "node-exact" {
		t.Fatalf("rendered node arguments = %v", spec.Cmd)
	}
}

func TestHostPreparationRunsBetweenCreateAndStart(t *testing.T) {
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"host-prep","version":"1"},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":[],
	    "hostPreparation":{"requireSwap":true,"swappiness":0,"dropPageCache":true},
	    "permissions":["host.memory-tuning"],
	    "resources":{"pids":64}
	  }]
	}`
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedHostTelemetry(t, "node", 16<<30, 60)
	h.seedRecipe(t, "recipe-host-prep", manifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{
		RecipeDigest: "recipe-host-prep",
		Placements:   []PlacementOverride{{NodeID: "node", Rank: 0}},
	})
	if err != nil || !plan.Ready {
		t.Fatalf("host preparation plan = %+v, err=%v", plan, err)
	}
	if len(plan.Images) != 1 || plan.Images[0].Digest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("image preflight = %+v", plan.Images)
	}
	if len(plan.HostPreparation) != 1 || plan.HostPreparation[0].SwapTotalBytes != 16<<30 ||
		plan.HostPreparation[0].SwappinessCurrent != 60 ||
		plan.HostPreparation[0].SwappinessTarget == nil || *plan.HostPreparation[0].SwappinessTarget != 0 {
		t.Fatalf("host preparation preflight = %+v", plan.HostPreparation)
	}
	deployment := h.createDeployment(t, "recipe-host-prep", PlacementOverride{NodeID: "node", Rank: 0})

	create := h.nodes.workloadCommands()[0].msg.GetWorkloadCommand()
	if create.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
		t.Fatalf("operation after acquisition = %s, want CREATE", create.GetOp())
	}
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: create.GetCommandId(), Ok: true})

	commands := h.nodes.workloadCommands()
	prepare := commands[len(commands)-1].msg.GetWorkloadCommand()
	if prepare.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_HOST_PREPARE {
		t.Fatalf("operation after create = %s, want HOST_PREPARE", prepare.GetOp())
	}
	if got := ParseDispatch(deploymentRow(t, h, deployment.ID).Dispatch).Get(0); got != PhaseHostPreparing {
		t.Fatalf("phase during host preparation = %s, want %s", got, PhaseHostPreparing)
	}
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(prepare.GetContainerSpec(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.HostPreparation == nil || !spec.HostPreparation.RequireSwap ||
		spec.HostPreparation.Swappiness == nil || *spec.HostPreparation.Swappiness != 0 ||
		!spec.HostPreparation.DropPageCache {
		t.Fatalf("host preparation spec = %+v", spec.HostPreparation)
	}

	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: prepare.GetCommandId(), Ok: true})
	start := h.nodes.workloadCommands()[len(h.nodes.workloadCommands())-1].msg.GetWorkloadCommand()
	if start.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_START {
		t.Fatalf("operation after host preparation = %s, want START", start.GetOp())
	}
}

func TestWorkersFirstPersistsHeadWaitAndStartsWorkerFirst(t *testing.T) {
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"worker-first","version":"1"},
	  "compatibility":{"nodeCount":2},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":[],"ranks":[0,1],"startOrder":"workers-first",
	    "resources":{"pids":64}
	  }]
	}`
	h := newHarness(t)
	h.seedNode(t, "head", nil, "")
	h.seedNode(t, "worker", nil, "")
	h.seedRecipe(t, "recipe-worker-first", manifest)
	deployment := h.createDeployment(t, "recipe-worker-first",
		PlacementOverride{NodeID: "head", Rank: 0},
		PlacementOverride{NodeID: "worker", Rank: 1},
	)

	for _, sent := range append([]sentMsg(nil), h.nodes.workloadCommands()...) {
		command := sent.msg.GetWorkloadCommand()
		if sent.nodeID == "head" && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
			h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
		}
	}
	if got := ParseDispatch(deploymentRow(t, h, deployment.ID).Dispatch).Get(0); got != PhaseHostPrepared {
		t.Fatalf("head wait phase = %s, want persisted %s", got, PhaseHostPrepared)
	}
	for _, sent := range h.nodes.workloadCommands() {
		if sent.nodeID == "head" && sent.msg.GetWorkloadCommand().GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
			t.Fatal("head started before worker")
		}
	}

	for _, sent := range append([]sentMsg(nil), h.nodes.workloadCommands()...) {
		command := sent.msg.GetWorkloadCommand()
		if sent.nodeID == "worker" && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
			h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
		}
	}
	var workerStart *agentv1.WorkloadCommand
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if sent.nodeID == "worker" && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
			workerStart = command
		}
	}
	if workerStart == nil {
		t.Fatal("worker START was not dispatched")
	}
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: workerStart.GetCommandId(), Ok: true})

	var headStart *agentv1.WorkloadCommand
	for _, sent := range h.nodes.workloadCommands() {
		command := sent.msg.GetWorkloadCommand()
		if sent.nodeID == "head" && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_START {
			headStart = command
		}
	}
	if headStart == nil {
		t.Fatal("head START was not dispatched after worker acknowledgement")
	}
}

// TestRenderedSpecSetsMemlockUlimitForRDMA reproduces the spark2 RoCE failure
// where ibv_reg_mr_iova2 returned ENOMEM: the workload drops all capabilities
// (no CAP_IPC_LOCK) and the default RLIMIT_MEMLOCK is too small for NCCL to
// register IB buffers. An RDMA workload must get memlock unlimited + 64 MiB
// stack, matching the prior Spark launcher; a non-RDMA workload must not.
func TestRenderedSpecSetsMemlockUlimitForRDMA(t *testing.T) {
	base := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"%s","version":"1"},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":[],"resources":{"pids":64}%s%s
	  }]
	}`

	run := func(name, extra, perm string, wantRDMA bool) {
		t.Run(name, func(t *testing.T) {
			manifest := fmt.Sprintf(base, name, extra, perm)
			h := newHarness(t)
			h.seedNode(t, "node-"+name, nil, "")
			h.seedRecipe(t, "recipe-"+name, manifest)
			h.createDeployment(t, "recipe-"+name, PlacementOverride{NodeID: "node-" + name, Rank: 0})
			commands := h.nodes.workloadCommands()
			if len(commands) != 1 {
				t.Fatalf("workload commands = %d", len(commands))
			}
			var spec runtime.ContainerSpec
			if err := json.Unmarshal(commands[0].msg.GetWorkloadCommand().GetContainerSpec(), &spec); err != nil {
				t.Fatal(err)
			}
			if wantRDMA {
				if len(spec.Ulimits) != 2 {
					t.Fatalf("RDMA spec ulimits = %+v, want 2 (memlock, stack)", spec.Ulimits)
				}
				var memlock, stack *runtime.Ulimit
				for i := range spec.Ulimits {
					switch spec.Ulimits[i].Name {
					case "memlock":
						memlock = &spec.Ulimits[i]
					case "stack":
						stack = &spec.Ulimits[i]
					}
				}
				if memlock == nil || memlock.Hard != -1 || memlock.Soft != -1 {
					t.Fatalf("memlock = %+v, want unlimited (-1)", memlock)
				}
				if stack == nil || stack.Hard != 67108864 || stack.Soft != 67108864 {
					t.Fatalf("stack = %+v, want 64MiB", stack)
				}
			} else {
				if len(spec.Ulimits) != 0 {
					t.Fatalf("non-RDMA spec ulimits = %+v, want none", spec.Ulimits)
				}
			}
		})
	}

	run("rdma-all", `,"devices":{"rdma":{"all":true}}`, `,"permissions":["devices.rdma"]`, true)
	run("plain", "", "", false)
}

func TestExtensionCommandsBracketWorkload(t *testing.T) {
	extension := `{
	  "image":{"reference":"helper:v1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	  "command":["helper"],"args":[],"outputSchema":{
	    "$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",
	    "properties":{"version":{"type":"integer"}},"required":["version"]
	  }
	}`
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"extensions","version":"1"},
	  "workloads":[{
	    "image":{"reference":"workload:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":[],"resources":{"pids":64}
	  }],"prepare":` + extension + `,"verify":` + extension + `
	}`
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe-extensions", manifest)
	deployment := h.createDeployment(t, "recipe-extensions", PlacementOverride{NodeID: "node", Rank: 0})
	extensions := h.nodes.extensionCommands()
	if len(extensions) != 1 || extensions[0].msg.GetExtensionCommand().GetPhase() != "prepare" ||
		len(h.nodes.workloadCommands()) != 0 {
		t.Fatalf("initial commands extensions=%d workloads=%d", len(extensions), len(h.nodes.workloadCommands()))
	}
	prepare := extensions[0].msg.GetExtensionCommand()
	var prepareSpec runtime.ContainerSpec
	if err := json.Unmarshal(prepare.GetContainerSpec(), &prepareSpec); err != nil {
		t.Fatal(err)
	}
	if prepareSpec.NetworkMode != "none" || !prepareSpec.ReadonlyRootfs || !prepareSpec.NoNewPrivileges ||
		prepareSpec.Labels[runtime.LabelModule] != "extension" {
		t.Fatalf("prepare sandbox = %+v", prepareSpec)
	}
	h.svc.mu.Lock()
	h.svc.inflight = map[string]*inflightCmd{}
	h.svc.mu.Unlock()
	// A reconnect while prepare is in progress re-drives the idempotent helper.
	h.svc.dispatchNext(context.Background(), deployment.ID, 0, h.svc.runIDFor(context.Background(), deployment.ID), h.svc.placementFor(context.Background(), deployment.ID, 0))
	extensions = h.nodes.extensionCommands()
	if len(extensions) != 2 || extensions[1].msg.GetExtensionCommand().GetPhase() != "prepare" {
		t.Fatalf("prepare recovery commands = %+v", extensions)
	}
	prepare = extensions[1].msg.GetExtensionCommand()
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{
		CommandId: prepare.GetCommandId(), Ok: true, OutputJson: []byte(`{\"version\":1}`),
	})
	workloads := h.nodes.workloadCommands()
	if len(workloads) != 1 || workloads[0].msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
		t.Fatalf("workloads after prepare = %+v", workloads)
	}
	for _, operation := range []agentv1.WorkloadOp{
		agentv1.WorkloadOp_WORKLOAD_OP_CREATE,
		agentv1.WorkloadOp_WORKLOAD_OP_START,
	} {
		commands := h.nodes.workloadCommands()
		command := commands[len(commands)-1].msg.GetWorkloadCommand()
		if command.GetOp() != operation {
			t.Fatalf("operation = %s, want %s", command.GetOp(), operation)
		}
		h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
	}
	extensions = h.nodes.extensionCommands()
	if len(extensions) != 3 || extensions[2].msg.GetExtensionCommand().GetPhase() != "verify" {
		t.Fatalf("verify extension commands = %+v", extensions)
	}
	verify := extensions[2].msg.GetExtensionCommand()
	h.svc.mu.Lock()
	h.svc.inflight = map[string]*inflightCmd{}
	h.svc.mu.Unlock()
	h.svc.dispatchNext(context.Background(), deployment.ID, 0, h.svc.runIDFor(context.Background(), deployment.ID), h.svc.placementFor(context.Background(), deployment.ID, 0))
	extensions = h.nodes.extensionCommands()
	if len(extensions) != 4 || extensions[3].msg.GetExtensionCommand().GetPhase() != "verify" {
		t.Fatalf("verify recovery commands = %+v", extensions)
	}
	verify = extensions[3].msg.GetExtensionCommand()
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{
		CommandId: verify.GetCommandId(), Ok: true, OutputJson: []byte(`{\"version\":1}`),
	})
	if got := ParseDispatch(deploymentRow(t, h, deployment.ID).Dispatch).Get(0); got != PhaseStarted {
		t.Fatalf("phase after verify = %s", got)
	}
	h.svc.setPhase(context.Background(), deployment.ID, 0, PhaseVerifying)
	if _, err := h.svc.Stop(context.Background(), deployment.ID); err != nil {
		t.Fatal(err)
	}
	extensions = h.nodes.extensionCommands()
	stopExtension := extensions[len(extensions)-1].msg.GetExtensionCommand()
	if stopExtension.GetPhase() != "stop" {
		t.Fatalf("extension stop = %+v", stopExtension)
	}
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: stopExtension.GetCommandId(), Ok: true})
	workloads = h.nodes.workloadCommands()
	if got := workloads[len(workloads)-1].msg.GetWorkloadCommand().GetOp(); got != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("workload operation after extension stop = %s", got)
	}
}

func TestVariantSelectionUsesActuallyPlacedAccelerator(t *testing.T) {
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"variant","version":"1"},
	  "compatibility":{"nodeCount":1,"accelerator":{"vendor":"nvidia","count":1}},
	  "workloads":[
	    {"match":{"accelerator":{"vendor":"nvidia","architectures":["sm_120"]}},
	     "image":{"reference":"sm120:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	     "command":["serve"],"args":[],"resources":{"pids":64}},
	    {"image":{"reference":"fallback:v1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	     "command":["serve"],"args":[],"resources":{"pids":64}}
	  ]
	}`
	h := newHarness(t)
	h.seedNode(t, "heterogeneous", []inventory.Accelerator{
		{Index: 0, UUID: "gpu-old", Vendor: "nvidia", Architecture: "sm_90", MemoryBytes: 16 << 30},
		{Index: 1, UUID: "gpu-match", Vendor: "nvidia", Architecture: "sm_120", MemoryBytes: 16 << 30},
	}, "")
	h.seedRecipe(t, "recipe-variant", manifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "recipe-variant"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorkloadIndex != 0 || len(plan.Placements) != 1 || plan.Placements[0].AcceleratorUUID != "gpu-match" {
		t.Fatalf("variant plan = %+v", plan)
	}
}

func TestWorkloadRestrictsCUDAVisibilityToAssignedGPU(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "mixed", []inventory.Accelerator{
		{Index: 0, UUID: "GPU-small", Vendor: "nvidia", Architecture: "sm_89", MemoryBytes: 24 << 30},
		{Index: 1, UUID: "GPU-selected", Vendor: "nvidia", Architecture: "sm_120", MemoryBytes: 96 << 30},
	}, "")
	h.seedRecipe(t, "recipe", `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"gpu","version":"1"},
	  "compatibility":{"nodeCount":1,"accelerator":{"vendor":"nvidia","architectures":["sm_120"],"count":1}},
	  "workloads":[{"image":{"reference":"test:latest"},"command":["serve"],"args":["--devices","${node.accelerators}"],
	    "resources":{"pids":64},
	    "devices":{"accelerator":{"all":true}},"env":{"CUDA_VISIBLE_DEVICES":"0"}}]
	}`)
	deployment := h.createDeployment(t, "recipe")
	placement := h.svc.placementFor(context.Background(), deployment.ID, 0)
	spec, err := h.svc.renderSpec(context.Background(), deployment.ID, 0, deployment.RunID, &placement)
	if err != nil {
		t.Fatal(err)
	}
	if spec.GPUsAll || len(spec.GPUDeviceIDs) != 1 || spec.GPUDeviceIDs[0] != "GPU-selected" {
		t.Fatalf("workload can escape its assigned accelerator: all=%v devices=%v", spec.GPUsAll, spec.GPUDeviceIDs)
	}
	if len(spec.Cmd) != 2 || spec.Cmd[1] != "GPU-selected" {
		t.Fatalf("native template does not retain the reviewed accelerator UUID: %v", spec.Cmd)
	}
	var visible []string
	for _, entry := range spec.Env {
		if strings.HasPrefix(entry, "CUDA_VISIBLE_DEVICES=") {
			visible = append(visible, entry)
		}
	}
	if len(visible) != 1 || visible[0] != "CUDA_VISIBLE_DEVICES=GPU-selected" {
		t.Fatalf("CUDA visibility does not enforce the assigned UUID: %v", visible)
	}
}

func deploymentRow(t *testing.T, h *harness, depID string) db.GetDeploymentRow {
	t.Helper()
	waitAcquisition(t, h.svc)
	row, err := h.q.GetDeployment(context.Background(), depID)
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	return row
}

func runState(t *testing.T, h *harness, runID string) string {
	t.Helper()
	run, err := h.svc.runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run.State
}

func transferState(t *testing.T, h *harness, transferID string) (state, diagnostic string) {
	t.Helper()
	tr, err := h.q.GetTransfer(context.Background(), transferID)
	if err != nil {
		t.Fatalf("get transfer: %v", err)
	}
	diagnostic = tr.Diagnostic.String
	return tr.State, diagnostic
}

func gpuAccs(id string) []inventory.Accelerator {
	return []inventory.Accelerator{
		{Index: 0, Vendor: "nvidia", Architecture: "sm_120", Name: "GB10", UUID: "GPU-" + id, MemoryBytes: 1 << 30},
	}
}

// ---------------------------------------------------------------- tests

// TestPlanCreatePersistsWorkloadIndex: the plan's selected workload variant
// is persisted in the placement document, selectWorkload honors it, and
// Create rejects a stale plan digest.
func TestPlanCreatePersistsWorkloadIndex(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "100.86.3.45:4433")
	h.seedRecipe(t, "recipe-1", noArtifactManifest)

	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "recipe-1"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.WorkloadIndex != 0 {
		t.Fatalf("workload index = %d, want 0", plan.WorkloadIndex)
	}
	if !plan.Ready {
		t.Fatalf("plan not ready: %v", plan.Diagnostics)
	}

	dep := h.createDeployment(t, "recipe-1")
	row := deploymentRow(t, h, dep.ID)
	ps := ParsePlacementSet(row.Placement)
	if ps.Workload == nil || *ps.Workload != 0 {
		t.Fatalf("persisted workload index = %v, want 0", ps.Workload)
	}
	if len(ps.Entries) != 1 || ps.Entries[0].NodeID != "node-a" {
		t.Fatalf("unexpected entries: %+v", ps.Entries)
	}

	// selectWorkload must honor the persisted index.
	m, err := h.svc.manifestFor(context.Background(), "recipe-1")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	wi, w, err := h.svc.selectWorkload(context.Background(), row, m)
	if err != nil || wi != 0 || w == nil {
		t.Fatalf("selectWorkload = (%d, %v, %v)", wi, w, err)
	}

	// A stale plan digest must be rejected.
	if _, err := h.createReviewed(context.Background(), CreateRequest{
		RecipeDigest: "recipe-1", PlanDigest: "sha256:stale",
	}); err == nil {
		t.Fatal("create with stale plan digest: want error")
	}
}

func TestPlanDigestIgnoresLivePreflightTelemetry(t *testing.T) {
	fabric := "spark-p2p"
	plan := Plan{
		RecipeDigest:  "sha256:recipe",
		Variants:      map[string]string{"model": "stable"},
		WorkloadIndex: 0,
		Placements: []Placement{
			{NodeID: "spark2", Rank: 0, AcceleratorUUID: "GPU-head"},
			{NodeID: "spark3", Rank: 1, AcceleratorUUID: "GPU-worker"},
		},
		Fabric:   &fabric,
		Ports:    []PortPreview{{NodeID: "spark2", HostPort: 8888, ContainerPort: 8888}},
		Endpoint: Endpoint{Host: "100.92.139.82", Port: 8888, Path: "/health"},
		Storage: []StoragePreview{
			{NodeID: "spark2", AvailableBytes: 1 << 40, Known: true, Sufficient: true},
		},
		HostPreparation: []HostPreparationPreview{
			{NodeID: "spark2", SwapTotalBytes: 16 << 30, SwappinessCurrent: 0},
		},
		Ready: true,
	}
	reviewed := plan.PlanDigest()

	plan.Storage[0].AvailableBytes -= 4096
	plan.Transfers = []TransferPreview{{ArtifactID: "model", SourceNode: "origin", DestNode: "spark2", Bytes: 200 << 30}}
	plan.HostPreparation[0].SwapTotalBytes += 4096
	plan.Diagnostics = []diag.Diagnostic{{Code: "telemetry.changed"}}
	if fresh := plan.PlanDigest(); fresh != reviewed {
		t.Fatalf("live preflight telemetry changed plan digest: %s != %s", fresh, reviewed)
	}

	plan.Endpoint.Port++
	if changed := plan.PlanDigest(); changed == reviewed {
		t.Fatal("launch contract change did not change plan digest")
	}
}

func TestPeerTransferUsesFabricAddressForWildcardListener(t *testing.T) {
	const identity = "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := newHarness(t)
	h.seedNode(t, "dest", gpuAccs("d"), "[::]:9444")
	h.seedNode(t, "src", gpuAccs("s"), "[::]:9444")
	h.seedArtifact(t, "art-1", identity)
	h.seedPlacement(t, "art-1", "src", "/var/lib/lmw/artifacts/model", "valid")

	bindings, err := json.Marshal([]fabriccfg.MemberBinding{
		{NodeID: "src", InterfaceName: "enx0", Address: "10.0.0.2"},
		{NodeID: "dest", InterfaceName: "enx0", Address: "10.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.CreateFabric(context.Background(), db.CreateFabricParams{
		ID: "fabric-1", Name: "spark-p2p", Transport: fabriccfg.TransportTCP,
		Members: `["src","dest"]`, Bindings: string(bindings), Version: "1",
	}); err != nil {
		t.Fatalf("create fabric: %v", err)
	}
	artifact, err := h.q.GetArtifact(context.Background(), "art-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.startTransfer(context.Background(), artifact, "src", "dest", "", "fabric-1", "run-1"); err != nil {
		t.Fatalf("start transfer: %v", err)
	}
	commands := h.nodes.transferCommands()
	if len(commands) != 1 || commands[0].msg.GetTransferCommand().GetPeerAddress() != "10.0.0.2:9444" {
		t.Fatalf("transfer commands = %+v, want fabric-routable source address", commands)
	}
}

// TestStopCompletesFromMissingRank: a rank whose container disappeared
// (state "missing") completes the stop: observed stopped, run cancelled,
// leases released.
func TestStopCompletesFromMissingRank(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")

	ctx := context.Background()
	row := deploymentRow(t, h, dep.ID)
	ps := ParsePlacementSet(row.Placement)
	acc := ps.Entries[0].AcceleratorUUID
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, observed_state='healthy' WHERE id=?",
		`{"0":"started"}`, dep.ID); err != nil {
		t.Fatalf("seed dispatch: %v", err)
	}
	var n int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource=? AND state='active'",
		"gpu:node-a:"+acc).Scan(&n); err != nil || n != 1 {
		t.Fatalf("active gpu leases = %d (err %v), want 1", n, err)
	}

	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stops := h.nodes.workloadCommands()
	last := stops[len(stops)-1]
	if last.msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("last workload op = %v, want STOP", last.msg.GetWorkloadCommand().GetOp())
	}

	// The agent reports the container is gone.
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, ContainerId: "c1", State: "missing", Rank: 0,
		DiagnosticCode: "container.missing",
	})

	row = deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopped" {
		t.Fatalf("observed = %s, want stopped", row.ObservedState)
	}
	if got := ParseDispatch(row.Dispatch).Get(0); got != PhaseStopped {
		t.Fatalf("rank 0 phase = %s, want stopped", got)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Cancelled) {
		t.Fatalf("run state = %s, want cancelled", got)
	}
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource=? AND state='active'",
		"gpu:node-a:"+acc).Scan(&n); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if n != 0 {
		t.Fatalf("active gpu leases = %d, want 0", n)
	}

	// A queued pre-stop observation must not regress a confirmed stop.
	commandCount := len(h.nodes.workloadCommands())
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, ContainerId: "c1", State: "created", Rank: 0,
	})
	row = deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopped" || ParseDispatch(row.Dispatch).Get(0) != PhaseStopped {
		t.Fatalf("stale created update regressed stop: %+v", row)
	}
	if got := len(h.nodes.workloadCommands()); got != commandCount {
		t.Fatalf("stale created update dispatched %d commands, want %d", got, commandCount)
	}

	// A genuinely active container after confirmation is driven back to stop.
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, ContainerId: "c1", State: "running", Rank: 0,
	})
	row = deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopping" || ParseDispatch(row.Dispatch).Get(0) != PhaseStopping {
		t.Fatalf("running update after stop = %+v, want stopping", row)
	}
	commands := h.nodes.workloadCommands()
	if got := commands[len(commands)-1].msg.GetWorkloadCommand().GetOp(); got != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("recovery workload op = %v, want STOP", got)
	}
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, ContainerId: "c1", State: "missing", Rank: 0,
	})
	if row = deploymentRow(t, h, dep.ID); row.ObservedState != "stopped" {
		t.Fatalf("recovered stop observed = %s, want stopped", row.ObservedState)
	}
}

func TestRestartRejectsPreviousRunAndUnscopedStateReports(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	driveDeploymentHealthy(t, h, dep.ID, "node-a")
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("stop original run: %v", err)
	}
	commands := h.nodes.workloadCommands()
	stop := commands[len(commands)-1].msg.GetWorkloadCommand()
	if stop.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("last command = %s, want STOP", stop.GetOp())
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: stop.GetCommandId(), Ok: true})
	if got := runState(t, h, dep.RunID); got != string(runs.Cancelled) {
		t.Fatalf("original run = %s, want cancelled", got)
	}

	restarted, err := h.svc.Start(ctx, dep.ID)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.ID != dep.ID || restarted.RunID == "" || restarted.RunID == dep.RunID {
		t.Fatalf("restart identity = deployment %s run %s", restarted.ID, restarted.RunID)
	}
	driveDeploymentHealthy(t, h, dep.ID, "node-a")
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: restarted.RunID, ContainerId: "container-new", State: "running", Rank: 0,
	})
	before := deploymentRow(t, h, dep.ID)
	placement := ParsePlacementSet(before.Placement).EntryFor(0)
	if before.DesiredState != "running" || before.ObservedState != "healthy" ||
		placement == nil || placement.Container != "container-new" {
		t.Fatalf("current-run running report did not establish healthy placement: %+v", before)
	}
	commandCount := len(h.nodes.workloadCommands())

	for _, report := range []struct {
		name  string
		runID string
		state string
	}{
		{name: "previous running", runID: dep.RunID, state: "running"},
		{name: "previous missing", runID: dep.RunID, state: "missing"},
		{name: "unscoped running", state: "running"},
		{name: "unscoped missing", state: "missing"},
	} {
		t.Run(report.name, func(t *testing.T) {
			h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
				DeploymentId:      dep.ID,
				RunId:             report.runID,
				ContainerId:       dep.ID,
				State:             report.state,
				Rank:              0,
				DiagnosticMessage: "retained runtime diagnostic",
			})
			after := deploymentRow(t, h, dep.ID)
			if after.DesiredState != "running" || after.ObservedState != "healthy" {
				t.Errorf("rejected report changed deployment state to %s/%s", after.DesiredState, after.ObservedState)
			}
			if after.Placement != before.Placement {
				t.Errorf("rejected report replaced current container placement: %s", after.Placement)
			}
			if after.Diagnostics != before.Diagnostics {
				t.Errorf("rejected report contaminated diagnostics: %s", after.Diagnostics)
			}
			if after.Dispatch != before.Dispatch || after.Endpoint != before.Endpoint {
				t.Errorf("rejected report changed dispatch or endpoint: %+v", after)
			}
			if got := runState(t, h, restarted.RunID); got != string(runs.Running) {
				t.Errorf("rejected report changed current run to %s", got)
			}
			if got := len(h.nodes.workloadCommands()); got != commandCount {
				t.Errorf("rejected report dispatched %d additional commands", got-commandCount)
			}
		})
	}

	// The same terminal report from the current run must still fail the workload.
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId:      dep.ID,
		RunId:             restarted.RunID,
		ContainerId:       "container-new",
		State:             "missing",
		Rank:              0,
		DiagnosticMessage: "current container disappeared",
	})
	after := deploymentRow(t, h, dep.ID)
	if after.DesiredState != "stopped" || after.ObservedState != "stopped" {
		t.Fatalf("current-run missing report left deployment %s/%s", after.DesiredState, after.ObservedState)
	}
	run, err := h.svc.runs.Get(ctx, restarted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != string(runs.Failed) || run.ErrorCode == nil || *run.ErrorCode != "workload.missing" ||
		run.ErrorMessage == nil || !strings.Contains(*run.ErrorMessage, "current container disappeared") {
		t.Fatalf("current-run missing report did not fail current run: %+v", run)
	}
	if strings.Contains(after.Diagnostics, "retained runtime diagnostic") {
		t.Fatalf("previous-run diagnostic leaked into failure: %s", after.Diagnostics)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Cancelled) {
		t.Fatalf("original run changed to %s, want cancelled", got)
	}
}

func TestRunningHeadBecomesHealthyOnlyAfterReadinessPasses(t *testing.T) {
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}

	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-readiness", strings.ReplaceAll(gpuManifest, "8000", port))
	deployment := h.createDeployment(t, "recipe-readiness")
	ctx := context.Background()
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, endpoint=? WHERE id=?",
		`{"0":"started"}`, parsed.Host, deployment.ID); err != nil {
		t.Fatal(err)
	}
	update := &agentv1.StateUpdate{
		DeploymentId: deployment.ID, RunId: deployment.RunID, ContainerId: "container-a", State: "running", Rank: 0,
	}
	h.svc.OnStateUpdate(ctx, "node-a", update)
	row := deploymentRow(t, h, deployment.ID)
	if row.ObservedState != "starting" {
		t.Fatalf("observed before readiness = %s, want starting", row.ObservedState)
	}

	ready.Store(true)
	h.svc.OnStateUpdate(ctx, "node-a", update)
	row = deploymentRow(t, h, deployment.ID)
	if row.ObservedState != "healthy" {
		t.Fatalf("observed after readiness = %s, want healthy", row.ObservedState)
	}
	run, err := h.svc.runs.Get(ctx, row.RunID.String)
	if err != nil {
		t.Fatal(err)
	}
	ranks, _ := run.Progress["ranks"].([]any)
	progress, _ := ranks[0].(map[string]any)
	if progress["phase"] != "healthy" {
		t.Fatalf("rank progress = %#v, want healthy", progress)
	}
}

func TestUnexpectedWorkloadTerminationFailsAndStopsDeployment(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		exitCode  int32
		oomKilled bool
		wantCode  string
	}{
		{name: "exited", state: "exited", exitCode: 137, wantCode: "workload.exited"},
		{name: "dead", state: "dead", exitCode: 255, wantCode: "workload.dead"},
		{name: "missing", state: "missing", wantCode: "workload.missing"},
		{name: "oom killed", state: "exited", exitCode: 137, oomKilled: true, wantCode: "workload.oom_killed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.seedNode(t, "node-a", gpuAccs("a"), "")
			h.seedRecipe(t, "recipe-gpu", gpuManifest)
			dep := h.createDeployment(t, "recipe-gpu")
			ctx := context.Background()

			row := deploymentRow(t, h, dep.ID)
			ps := ParsePlacementSet(row.Placement)
			resource := "gpu:node-a:" + ps.Entries[0].AcceleratorUUID
			if _, err := h.dbh.ExecContext(ctx,
				"UPDATE deployments SET dispatch=?, observed_state='healthy', endpoint='100.86.3.45:8000' WHERE id=?",
				`{"0":"started"}`, dep.ID); err != nil {
				t.Fatalf("seed running deployment: %v", err)
			}

			update := &agentv1.StateUpdate{
				DeploymentId:      dep.ID,
				RunId:             dep.RunID,
				ContainerId:       "container-a",
				State:             tt.state,
				Rank:              0,
				ExitCode:          tt.exitCode,
				OomKilled:         tt.oomKilled,
				DiagnosticMessage: "runtime failure",
			}
			h.svc.OnStateUpdate(ctx, "node-a", update)
			h.svc.OnStateUpdate(ctx, "node-a", update)

			row = deploymentRow(t, h, dep.ID)
			placement := ParsePlacementSet(row.Placement).EntryFor(0)
			if placement == nil || placement.Container != "container-a" {
				t.Fatalf("placement container = %+v, want container-a", placement)
			}
			if row.DesiredState != "stopped" || row.ObservedState != "stopped" {
				t.Fatalf("deployment state = %s/%s, want stopped/stopped", row.DesiredState, row.ObservedState)
			}
			if row.Endpoint.Valid {
				t.Fatalf("endpoint = %q, want NULL", row.Endpoint.String)
			}
			if got := ParseDispatch(row.Dispatch).Get(0); got != PhaseStopped {
				t.Fatalf("rank phase = %s, want stopped", got)
			}
			diagnostics := diag.Decode(row.Diagnostics)
			if len(diagnostics) != 1 || diagnostics[0].Code != tt.wantCode ||
				diagnostics[0].Resource == nil || *diagnostics[0].Resource != "rank:0" {
				t.Fatalf("diagnostics = %+v", diagnostics)
			}

			run, err := h.svc.runs.Get(ctx, dep.RunID)
			if err != nil {
				t.Fatalf("get failed run: %v", err)
			}
			if run.State != string(runs.Failed) || run.ErrorCode == nil || *run.ErrorCode != tt.wantCode {
				t.Fatalf("run = %+v", run)
			}
			for _, part := range []string{"rank 0", "container container-a", "state=" + tt.state, "oom_killed="} {
				if run.ErrorMessage == nil || !strings.Contains(*run.ErrorMessage, part) {
					t.Fatalf("error message = %v, missing %q", run.ErrorMessage, part)
				}
			}
			if tt.state != "missing" && !strings.Contains(*run.ErrorMessage, fmt.Sprintf("exit_code=%d", tt.exitCode)) {
				t.Fatalf("error message = %q, missing exit code", *run.ErrorMessage)
			}

			var active int
			if err := h.dbh.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM leases WHERE resource=? AND state='active'", resource).Scan(&active); err != nil {
				t.Fatalf("count leases: %v", err)
			}
			if active != 0 {
				t.Fatalf("active leases = %d, want 0", active)
			}
		})
	}
}

func TestUnexpectedRankFailureHoldsLeasesUntilPeersStop(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedNode(t, "node-b", gpuAccs("b"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()

	row := deploymentRow(t, h, dep.ID)
	placements := ParsePlacementSet(row.Placement)
	placements.Entries = append(placements.Entries, Placement{
		NodeID:           "node-b",
		NodeName:         "node-b",
		Rank:             1,
		AcceleratorIndex: 0,
		AcceleratorUUID:  "GPU-b",
		Accelerators:     []string{"GPU-b"},
	})
	placements.Ranks["node-b"] = 1
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET placement=?, dispatch=?, observed_state='healthy', endpoint='100.86.3.45:8000' WHERE id=?",
		placements.Marshal(), `{"0":"started","1":"started"}`, dep.ID); err != nil {
		t.Fatalf("seed multi-rank deployment: %v", err)
	}
	if _, err := h.dbh.ExecContext(ctx,
		"INSERT INTO leases(resource, owner_kind, owner_id) VALUES(?, 'deployment', ?)",
		"gpu:node-b:GPU-b", dep.ID); err != nil {
		t.Fatalf("seed peer lease: %v", err)
	}

	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID,
		RunId:        dep.RunID,
		ContainerId:  "container-a",
		State:        "exited",
		Rank:         0,
		ExitCode:     1,
	})

	row = deploymentRow(t, h, dep.ID)
	if row.DesiredState != "stopped" || row.ObservedState != "stopping" {
		t.Fatalf("deployment state = %s/%s, want stopped/stopping", row.DesiredState, row.ObservedState)
	}
	if got := ParseDispatch(row.Dispatch).Get(1); got != PhaseStopping {
		t.Fatalf("peer phase = %s, want stopping", got)
	}
	var active int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE owner_kind='deployment' AND owner_id=? AND state='active' AND resource LIKE 'gpu:%'",
		dep.ID).Scan(&active); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if active != 2 {
		t.Fatalf("active leases after first crash = %d, want 2", active)
	}
	commands := h.nodes.workloadCommands()
	last := commands[len(commands)-1]
	if last.nodeID != "node-b" || last.msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("peer stop command = node %s op %s", last.nodeID, last.msg.GetWorkloadCommand().GetOp())
	}

	h.svc.OnStateUpdate(ctx, "node-b", &agentv1.StateUpdate{
		DeploymentId: dep.ID,
		RunId:        dep.RunID,
		ContainerId:  "container-b",
		State:        "missing",
		Rank:         1,
	})

	row = deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopped" {
		t.Fatalf("observed = %s, want stopped", row.ObservedState)
	}
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE owner_kind='deployment' AND owner_id=? AND state='active' AND resource LIKE 'gpu:%'",
		dep.ID).Scan(&active); err != nil {
		t.Fatalf("count released leases: %v", err)
	}
	if active != 0 {
		t.Fatalf("active leases after peer stop = %d, want 0", active)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Failed) {
		t.Fatalf("run state = %s, want failed", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestStartReDrivesStoppedAndDeleteFreesSlot exercises the reverse
// lifecycle: a fully-stopped deployment can be started again (fresh run,
// re-acquired leases, re-dispatched from the beginning), a running
// deployment rejects start, and a stopped deployment can be deleted to
// free its slot (deployment and its runs removed).
func TestStartReDrivesStoppedAndDeleteFreesSlot(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "100.86.3.45:4433")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()

	row := deploymentRow(t, h, dep.ID)
	ps := ParsePlacementSet(row.Placement)
	acc := ps.Entries[0].AcceleratorUUID
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, observed_state='healthy' WHERE id=?",
		`{"0":"started"}`, dep.ID); err != nil {
		t.Fatalf("seed dispatch: %v", err)
	}
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: dep.RunID, ContainerId: "c1", State: "missing", Rank: 0,
		DiagnosticCode: "container.missing",
	})
	row = deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopped" {
		t.Fatalf("observed = %s, want stopped", row.ObservedState)
	}

	// --- Start ---
	started, err := h.svc.Start(ctx, dep.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.DesiredState != "running" {
		t.Fatalf("desired = %s, want running", started.DesiredState)
	}
	if started.RunID == dep.RunID {
		t.Fatalf("start did not create a fresh run")
	}
	row = deploymentRow(t, h, dep.ID)
	if got := ParseDispatch(row.Dispatch).Get(0); got != PhasePulled {
		t.Fatalf("rank 0 phase after start = %s, want inspected resources", got)
	}
	var n int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource=? AND state='active'",
		"gpu:node-a:"+acc).Scan(&n); err != nil || n != 1 {
		t.Fatalf("active gpu leases = %d (err %v), want 1", n, err)
	}
	cmds := h.nodes.workloadCommands()
	if last := cmds[len(cmds)-1].msg.GetWorkloadCommand().GetOp(); last != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
		t.Fatalf("last workload op after start = %v, want CREATE without PULL", last)
	}

	// Starting an already-running deployment is rejected.
	if _, err := h.svc.Start(ctx, dep.ID); err == nil {
		t.Fatalf("start of running deployment should fail")
	}

	// Deleting a running deployment is rejected.
	if err := h.svc.Delete(ctx, dep.ID); err == nil {
		t.Fatalf("delete of running deployment should fail")
	}

	// --- Stop again, then Delete ---
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId: dep.ID, RunId: started.RunID, ContainerId: "c1", State: "missing", Rank: 0,
		DiagnosticCode: "container.missing",
	})
	if err := h.svc.Delete(ctx, dep.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := h.svc.Get(ctx, dep.ID); err != ErrUnknown {
		t.Fatalf("Get after delete err = %v, want ErrUnknown", err)
	}
	var nr int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE deployment_id=?", dep.ID).Scan(&nr); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if nr != 0 {
		t.Fatalf("runs remaining = %d, want 0", nr)
	}
}

func TestStopCompletesBeforeReturningForPreContainerPhases(t *testing.T) {
	for _, phase := range []string{PhaseNone, PhasePrepared} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			h.seedNode(t, "node-a", gpuAccs("a"), "")
			h.seedRecipe(t, "recipe-gpu", gpuManifest)
			dep := h.createDeployment(t, "recipe-gpu")
			ctx := context.Background()
			dispatch, _ := json.Marshal(dispatchPhases{0: phase})
			if _, err := h.dbh.ExecContext(ctx,
				"UPDATE deployments SET dispatch=?, observed_state='healthy', endpoint='100.86.3.45:8000' WHERE id=?",
				string(dispatch), dep.ID); err != nil {
				t.Fatalf("seed phase: %v", err)
			}

			stopped, err := h.svc.Stop(ctx, dep.ID)
			if err != nil {
				t.Fatalf("stop: %v", err)
			}
			if stopped.DesiredState != "stopped" || stopped.ObservedState != "stopped" {
				t.Fatalf("deployment = %s/%s, want stopped/stopped", stopped.DesiredState, stopped.ObservedState)
			}
			row := deploymentRow(t, h, dep.ID)
			if row.Endpoint.Valid {
				t.Fatalf("endpoint = %q, want NULL", row.Endpoint.String)
			}
			if got := ParseDispatch(row.Dispatch).Get(0); got != PhaseStopped {
				t.Fatalf("phase = %s, want stopped", got)
			}
		})
	}
}

func TestSynchronousStopAckCannotRegressStoppedPhase(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, observed_state='healthy' WHERE id=?",
		`{"0":"started"}`, dep.ID); err != nil {
		t.Fatalf("seed started phase: %v", err)
	}
	h.nodes.onSend = func(message *agentv1.ServerMessage) {
		command := message.GetWorkloadCommand()
		if command == nil || command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
			return
		}
		h.svc.OnCommandResult(ctx, &agentv1.CommandResult{
			CommandId:      command.GetCommandId(),
			Ok:             true,
			ContainerState: "exited",
		})
	}

	stopped, err := h.svc.Stop(ctx, dep.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.ObservedState != "stopped" {
		t.Fatalf("observed = %s, want stopped", stopped.ObservedState)
	}
	if got := ParseDispatch(deploymentRow(t, h, dep.ID).Dispatch).Get(0); got != PhaseStopped {
		t.Fatalf("phase = %s, want stopped", got)
	}
}

func TestRepeatedStopRedrivesUnresolvedRank(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, observed_state='healthy' WHERE id=?",
		`{"0":"started"}`, dep.ID); err != nil {
		t.Fatalf("seed started phase: %v", err)
	}
	h.nodes.setOnline("node-a", false)

	first, err := h.svc.Stop(ctx, dep.ID)
	if err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if first.ObservedState != "stopping" {
		t.Fatalf("first observed = %s, want stopping", first.ObservedState)
	}
	if got := ParseDispatch(deploymentRow(t, h, dep.ID).Dispatch).Get(0); got != PhaseStopping {
		t.Fatalf("offline phase = %s, want stopping", got)
	}
	if _, err := h.svc.Start(ctx, dep.ID); err == nil {
		t.Fatal("start while stopping should fail")
	}
	var active int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE owner_kind='deployment' AND owner_id=? AND state='active'",
		dep.ID).Scan(&active); err != nil {
		t.Fatalf("count held leases: %v", err)
	}
	if active == 0 {
		t.Fatal("offline stop released leases")
	}

	h.nodes.setOnline("node-a", true)
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("retry stop: %v", err)
	}
	commands := h.nodes.workloadCommands()
	last := commands[len(commands)-1].msg.GetWorkloadCommand()
	if last.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("retry op = %s, want STOP", last.GetOp())
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: last.GetCommandId(), Ok: true})
	if got := deploymentRow(t, h, dep.ID).ObservedState; got != "stopped" {
		t.Fatalf("observed after ack = %s, want stopped", got)
	}
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("idempotent stopped call: %v", err)
	}
}

func TestRejectedStopReportsDiagnosticAndRetainsResourcesUntilRetry(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	driveDeploymentHealthy(t, h, dep.ID, "node-a")

	placement := ParsePlacementSet(deploymentRow(t, h, dep.ID).Placement).Entries[0]
	activeGPULeases := func() int {
		t.Helper()
		var active int
		if err := h.dbh.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM leases WHERE resource=? AND owner_kind='deployment' AND owner_id=? AND state='active'",
			"gpu:node-a:"+placement.AcceleratorUUID, dep.ID).Scan(&active); err != nil {
			t.Fatalf("count GPU leases: %v", err)
		}
		return active
	}
	if got := activeGPULeases(); got != 1 {
		t.Fatalf("active GPU leases before stop = %d, want 1", got)
	}
	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	commands := h.nodes.workloadCommands()
	stop := commands[len(commands)-1].msg.GetWorkloadCommand()
	if stop.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
		t.Fatalf("last operation = %s, want STOP", stop.GetOp())
	}
	runBeforeRejection := runState(t, h, dep.RunID)
	const rejection = "refusing to stop containers owned by another installation"
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{
		CommandId: stop.GetCommandId(), Ok: false, Error: rejection,
	})

	view, err := h.svc.Get(ctx, dep.ID)
	if err != nil {
		t.Fatalf("deployment view: %v", err)
	}
	if view.DesiredState != "stopped" || view.ObservedState != "stopping" {
		t.Fatalf("rejected stop state = %s/%s, want stopped/stopping", view.DesiredState, view.ObservedState)
	}
	found := false
	for _, diagnostic := range view.Diagnostics {
		if diagnostic.Code != "workload.stop_failed" {
			continue
		}
		found = true
		if diagnostic.Severity != "error" || !strings.Contains(diagnostic.Message, rejection) ||
			diagnostic.Resource == nil || *diagnostic.Resource != "rank:0" {
			t.Fatalf("stop failure diagnostic = %+v", diagnostic)
		}
	}
	if !found {
		t.Fatalf("deployment diagnostics omit rejected stop: %+v", view.Diagnostics)
	}
	if got := ParseDispatch(deploymentRow(t, h, dep.ID).Dispatch).Get(0); got != PhaseStopping {
		t.Fatalf("rejected stop phase = %s, want stopping", got)
	}
	if got := runState(t, h, dep.RunID); got != runBeforeRejection {
		t.Fatalf("rejected stop changed run state from %s to %s", runBeforeRejection, got)
	}
	if got := activeGPULeases(); got != 1 {
		t.Fatalf("rejected stop released GPU lease: active = %d, want 1", got)
	}
	if got := len(h.nodes.workloadCommands()); got != len(commands) {
		t.Fatalf("rejected stop implicitly dispatched a retry: commands = %d, want %d", got, len(commands))
	}

	if _, err := h.svc.Stop(ctx, dep.ID); err != nil {
		t.Fatalf("explicit stop retry: %v", err)
	}
	retried := h.nodes.workloadCommands()
	if len(retried) != len(commands)+1 {
		t.Fatalf("explicit retry command count = %d, want %d", len(retried), len(commands)+1)
	}
	retry := retried[len(retried)-1].msg.GetWorkloadCommand()
	if retry.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP || retry.GetCommandId() == stop.GetCommandId() {
		t.Fatalf("explicit retry did not send a fresh STOP: %+v", retry)
	}
	if got := activeGPULeases(); got != 1 {
		t.Fatalf("retry released unconfirmed GPU lease: active = %d, want 1", got)
	}
	h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: retry.GetCommandId(), Ok: true})
	row := deploymentRow(t, h, dep.ID)
	if row.DesiredState != "stopped" || row.ObservedState != "stopped" || ParseDispatch(row.Dispatch).Get(0) != PhaseStopped {
		t.Fatalf("confirmed retry did not finish stop: %+v", row)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Cancelled) {
		t.Fatalf("confirmed stop run state = %s, want cancelled", got)
	}
	if got := activeGPULeases(); got != 0 {
		t.Fatalf("confirmed stop retained GPU lease: active = %d, want 0", got)
	}
}

func TestConvergeRepairsAllStoppedLegacyDeployment(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	if err := h.svc.runs.Cancel(ctx, dep.RunID); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET desired_state='stopped', observed_state='stopping', dispatch=?, endpoint='stale:8000' WHERE id=?",
		`{"0":"stopped"}`, dep.ID); err != nil {
		t.Fatalf("seed legacy stopping row: %v", err)
	}

	h.svc.Converge(ctx, "node-a")

	row := deploymentRow(t, h, dep.ID)
	if row.ObservedState != "stopped" {
		t.Fatalf("observed = %s, want stopped", row.ObservedState)
	}
	if row.Endpoint.Valid {
		t.Fatalf("endpoint = %q, want NULL", row.Endpoint.String)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Cancelled) {
		t.Fatalf("run = %s, want cancelled", got)
	}
	var active int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE owner_kind='deployment' AND owner_id=? AND state='active'",
		dep.ID).Scan(&active); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	if active != 0 {
		t.Fatalf("active leases = %d, want 0", active)
	}
}

func TestConvergeClearsLegacyFullyStoppedEndpoint(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET desired_state='stopped', observed_state='stopped', dispatch=?, endpoint='stale:8000' WHERE id=?",
		`{"0":"stopped"}`, dep.ID); err != nil {
		t.Fatalf("seed fully stopped row: %v", err)
	}

	h.svc.Converge(ctx, "node-a")

	row := deploymentRow(t, h, dep.ID)
	if row.Endpoint.Valid {
		t.Fatalf("endpoint = %q, want NULL", row.Endpoint.String)
	}
}

func TestStartPersistsReplannedPlacementAndRetainsFailedRun(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("old"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	dep := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()

	original := deploymentRow(t, h, dep.ID)
	if _, err := h.dbh.ExecContext(ctx,
		"UPDATE deployments SET dispatch=?, observed_state='healthy', endpoint='stale:9999', model_capabilities=? WHERE id=?",
		`{"0":"started"}`, `{"old":true}`, dep.ID); err != nil {
		t.Fatalf("seed running deployment: %v", err)
	}
	h.svc.OnStateUpdate(ctx, "node-a", &agentv1.StateUpdate{
		DeploymentId:      dep.ID,
		RunId:             dep.RunID,
		ContainerId:       "container-old",
		State:             "exited",
		Rank:              0,
		ExitCode:          1,
		DiagnosticMessage: "serve crashed",
	})
	failedRun, err := h.svc.runs.Get(ctx, dep.RunID)
	if err != nil {
		t.Fatalf("get failed run: %v", err)
	}
	if failedRun.State != string(runs.Failed) {
		t.Fatalf("old run state = %s, want failed", failedRun.State)
	}

	if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{
		ID:        "node-a",
		Inventory: nullString(inventoryWith(gpuAccs("new"), "")),
	}); err != nil {
		t.Fatalf("replace inventory: %v", err)
	}
	started, err := h.svc.Start(ctx, dep.ID)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if started.ID != dep.ID || started.RunID == dep.RunID {
		t.Fatalf("restart identity = deployment %s run %s", started.ID, started.RunID)
	}

	row := deploymentRow(t, h, dep.ID)
	replanned := ParsePlacementSet(row.Placement)
	if len(replanned.Entries) != 1 || replanned.Entries[0].AcceleratorUUID != "GPU-new" {
		t.Fatalf("persisted placement = %+v, want GPU-new", replanned.Entries)
	}
	if row.Parameters != original.Parameters {
		t.Fatalf("parameters changed from %q to %q", original.Parameters, row.Parameters)
	}
	if row.DesiredState != "running" || row.ObservedState != "unknown" {
		t.Fatalf("state = %s/%s, want running/unknown", row.DesiredState, row.ObservedState)
	}
	if row.Endpoint.String == "stale:9999" || !strings.Contains(row.Endpoint.String, ":8000") {
		t.Fatalf("endpoint = %q, want replanned port", row.Endpoint.String)
	}
	if row.Diagnostics != "[]" {
		t.Fatalf("diagnostics = %q, want []", row.Diagnostics)
	}
	if row.ModelCapabilities.Valid {
		t.Fatalf("model capabilities = %q, want NULL", row.ModelCapabilities.String)
	}

	var oldActive, newActive int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource='gpu:node-a:GPU-old' AND state='active'").Scan(&oldActive); err != nil {
		t.Fatalf("count old lease: %v", err)
	}
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource='gpu:node-a:GPU-new' AND state='active' AND owner_id=?",
		dep.ID).Scan(&newActive); err != nil {
		t.Fatalf("count new lease: %v", err)
	}
	if oldActive != 0 || newActive != 1 {
		t.Fatalf("active GPU leases old/new = %d/%d, want 0/1", oldActive, newActive)
	}
	if current, err := h.svc.runs.Get(ctx, started.RunID); err != nil || current.State != string(runs.Waiting) {
		t.Fatalf("new run = %+v err=%v, want waiting", current, err)
	}
	if retained, err := h.svc.runs.Get(ctx, dep.RunID); err != nil || retained.State != string(runs.Failed) {
		t.Fatalf("retained failed run = %+v err=%v", retained, err)
	}
}

func TestPlanNamesDeploymentOccupyingCompatibleGPU(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node-a", gpuAccs("a"), "")
	h.seedRecipe(t, "recipe-gpu", gpuManifest)
	occupant := h.createDeployment(t, "recipe-gpu")
	ctx := context.Background()

	blocked, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "recipe-gpu"})
	if err != nil {
		t.Fatalf("blocked plan: %v", err)
	}
	if blocked.Ready {
		t.Fatal("blocked plan is ready")
	}
	foundCapacity := false
	for _, diagnostic := range blocked.Diagnostics {
		if diagnostic.Code == "placement.no_capacity" {
			foundCapacity = true
		}
	}
	if !foundCapacity {
		t.Fatalf("diagnostics = %+v, missing placement.no_capacity", blocked.Diagnostics)
	}
	if len(blocked.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one GPU owner", blocked.Conflicts)
	}
	conflict := blocked.Conflicts[0]
	if conflict.Resource != "gpu:node-a:GPU-a" ||
		conflict.OccupiedBy != occupant.ID || conflict.DeploymentID != occupant.ID {
		t.Fatalf("conflict = %+v", conflict)
	}

	if _, err := h.svc.Stop(ctx, occupant.ID); err != nil {
		t.Fatalf("stop occupant: %v", err)
	}
	waitAcquisition(t, h.svc)
	for _, command := range h.nodes.workloadCommands() {
		workload := command.msg.GetWorkloadCommand()
		if workload.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_STOP {
			h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: workload.GetCommandId(), Ok: true})
		}
	}
	ready, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "recipe-gpu"})
	if err != nil {
		t.Fatalf("ready plan: %v", err)
	}
	if !ready.Ready || len(ready.Placements) != 1 {
		t.Fatalf("plan after stop = ready %t placements %+v diagnostics %+v conflicts %+v",
			ready.Ready, ready.Placements, ready.Diagnostics, ready.Conflicts)
	}
}

func TestLaunchProfileCRUDValidationAndPlanApplication(t *testing.T) {
	const manifest = `{
		"apiVersion": "lmw.dev/v1",
		"kind": "Recipe",
		"metadata": {"name": "settings", "version": "1"},
		"artifacts": [
			{
				"name": "model", "kind": "model", "defaultVariant": "small",
				"variants": [
					{"name": "small", "source": {"type": "huggingface", "identity": "hf://Acme/Small", "revision": "1111111111111111111111111111111111111111"}},
					{"name": "large", "source": {"type": "huggingface", "identity": "hf://Acme/Large", "revision": "2222222222222222222222222222222222222222"}}
				],
				"mount": "/models/model"
			},
			{
				"name": "drafter", "kind": "model", "defaultVariant": "tiny",
				"variants": [
					{"name": "tiny", "source": {"type": "huggingface", "identity": "hf://Acme/Tiny", "revision": "3333333333333333333333333333333333333333"}},
					{"name": "full", "source": {"type": "huggingface", "identity": "hf://Acme/Full", "revision": "4444444444444444444444444444444444444444"}}
				],
				"mount": "/models/drafter"
			}
		],
		"parameters": [
			{"name": "kv_cache", "type": "enum", "default": "fp8", "enum": ["fp8", "bf16"]}
		],
		"workloads": [{
			"image": {"reference": "test-serve:latest"},
			"command": ["serve"],
			"args": ["--kv-cache", "${setting.kv_cache}"],
			"resources": {"pids": 64},
			"ports": [{"container": 8000}]
		}]
	}`

	h := newHarness(t)
	h.seedNode(t, "dest", nil, "")
	h.seedRecipe(t, "settings-v1", manifest)
	h.seedRecipe(t, "settings-v2", manifest)
	ctx := context.Background()

	profile, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{
		Name: "fast", RecipeDigest: "settings-v1",
		Variants:   map[string]string{"model": "small", "drafter": "tiny"},
		Parameters: map[string]any{"kv_cache": "fp8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "fast" || profile.RecipeDigest != "settings-v1" {
		t.Fatalf("created profile = %+v", profile)
	}
	listed, err := h.svc.ListLaunchProfiles(ctx, "settings-v1")
	if err != nil || len(listed) != 1 || listed[0].ID != profile.ID {
		t.Fatalf("listed profiles = %+v, err=%v", listed, err)
	}

	first, err := h.svc.Plan(ctx, PlanRequest{
		RecipeDigest: "settings-v1", LaunchProfileID: profile.ID,
		Placements: []PlacementOverride{{NodeID: "dest", Rank: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Variants["model"] != "small" || first.Variants["drafter"] != "tiny" ||
		first.Parameters["kv_cache"] != "fp8" {
		t.Fatalf("applied parameters = variants:%+v parameters:%+v", first.Variants, first.Parameters)
	}

	updated, err := h.svc.UpdateLaunchProfile(ctx, profile.ID, UpsertLaunchProfileRequest{
		Name:       "quality",
		Variants:   map[string]string{"model": "large", "drafter": "full"},
		Parameters: map[string]any{"kv_cache": "bf16"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "quality" || updated.Variants["model"] != "large" {
		t.Fatalf("updated profile = %+v", updated)
	}
	second, err := h.svc.Plan(ctx, PlanRequest{
		RecipeDigest: "settings-v1", LaunchProfileID: profile.ID,
		Placements: []PlacementOverride{{NodeID: "dest", Rank: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest == first.Digest || second.Variants["model"] != "large" ||
		second.Parameters["kv_cache"] != "bf16" {
		t.Fatalf("updated plan = %+v; first digest = %s", second, first.Digest)
	}

	if _, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{
		Name: "unknown-variant", RecipeDigest: "settings-v1",
		Variants: map[string]string{"model": "missing"},
	}); err == nil || !strings.Contains(err.Error(), "has no variant") {
		t.Fatalf("unknown variant error = %v", err)
	}
	if _, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{
		Name: "bad-enum", RecipeDigest: "settings-v1",
		Parameters: map[string]any{"kv_cache": "int4"},
	}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("enum validation error = %v", err)
	}
	other, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{
		Name: "other", RecipeDigest: "settings-v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Plan(ctx, PlanRequest{
		RecipeDigest: "settings-v1", LaunchProfileID: other.ID,
		Placements: []PlacementOverride{{NodeID: "dest", Rank: 0}},
	}); err == nil || !strings.Contains(err.Error(), "different recipe digest") {
		t.Fatalf("cross-digest profile error = %v", err)
	}
	if _, err := h.svc.Plan(ctx, PlanRequest{
		RecipeDigest: "settings-v1", LaunchProfileID: profile.ID,
		Variants: map[string]string{"model": "small"},
	}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("profile/override exclusivity error = %v", err)
	}

	if err := h.svc.DeleteLaunchProfile(ctx, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.GetLaunchProfile(ctx, profile.ID); err == nil {
		t.Fatal("deleted profile remained readable")
	}
	if err := h.svc.DeleteLaunchProfile(ctx, profile.ID); err == nil {
		t.Fatal("deleting an unknown profile succeeded")
	}
}
