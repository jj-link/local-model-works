package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
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
}

type sentMsg struct {
	nodeID string
	msg    *agentv1.ServerMessage
}

func (f *fakeNodes) Send(nodeID string, m *agentv1.ServerMessage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online[nodeID] {
		return false
	}
	f.msgs = append(f.msgs, sentMsg{nodeID: nodeID, msg: m})
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
	caCA, err := ca.New()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	svc := New(sqlDB, q, bus, runs.New(sqlDB, q, bus, t.TempDir()), nodes, caCA)
	return &harness{svc: svc, nodes: nodes, q: q, dbh: sqlDB}
}

func inventoryWith(accs []inventory.Accelerator, peerListen string) string {
	b, _ := json.Marshal(inventory.Inventory{
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

func (h *harness) seedRecipe(t *testing.T, digest, manifest string) {
	t.Helper()
	ctx := context.Background()
	if err := h.q.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: "test", Version: "1",
		Source: "{}", TrustState: "local", Manifest: manifest,
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
		"resources": {"cpu": 1, "memoryBytes": 16777216, "pids": 64},
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
		"resources": {"cpu": 1, "memoryBytes": 16777216, "pids": 64},
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
		"resources": {"cpu": 1, "memoryBytes": 16777216, "pids": 64},
		"ports": [{"container": 8000}],
		"readiness": {"httpGet": {"path": "/health", "port": 8000}}
	}]
}`

func TestDeploymentModelAndEngineViews(t *testing.T) {
	tests := []struct {
		name          string
		profile       string
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
			name:    "profile override",
			profile: "fast",
			manifest: strings.Replace(
				strings.Replace(
					noArtifactManifest,
					`"metadata": {"name": "test-serve", "version": "1"}`,
					`"metadata": {"name": "test-serve", "version": "1", "model": "metadata-model", "engine": "vllm"}`,
					1,
				),
				`"workloads":`,
				`"parameters": [{"name": "model", "type": "string"}], "profiles": {"fast": {"model": "profile-model"}}, "workloads":`,
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
				Profile:      tt.profile,
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plan.Endpoint.Model != tt.expectedModel {
				t.Fatalf("plan endpoint model = %q, want %q", plan.Endpoint.Model, tt.expectedModel)
			}

			deployment, err := h.svc.Create(context.Background(), CreateRequest{
				RecipeDigest: "recipe-model",
				Profile:      tt.profile,
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

func (h *harness) createDeployment(t *testing.T, digest string, overrides ...PlacementOverride) *Deployment {
	t.Helper()
	dep, err := h.svc.Create(context.Background(), CreateRequest{
		RecipeDigest: digest, Placements: overrides,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return dep
}

func TestRenderedSpecUsesNodeIdentityAndHardeningDefaults(t *testing.T) {
	manifest := `{
	  "apiVersion":"lmw.dev/v1","kind":"Recipe","metadata":{"name":"hard","version":"1"},
	  "workloads":[{
	    "image":{"reference":"example:v1","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	    "command":["serve"],"args":["--node","${node.id}","--addr","${node.address}"],
	    "resources":{"cpu":2,"memoryBytes":33554432,"tmpfsBytes":67108864,"pids":128}
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
		spec.CPU != 2 || spec.MemoryBytes != 33554432 || spec.PidsLimit != 128 || spec.TmpfsBytes != 67108864 {
		t.Fatalf("hardened spec = %+v", spec)
	}
	if len(spec.Cmd) < 4 || spec.Cmd[1] != "node-exact" {
		t.Fatalf("rendered node arguments = %v", spec.Cmd)
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
	    "command":["serve"],"args":[],"resources":{"cpu":1,"memoryBytes":16777216,"pids":64}%s%s
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
	    "command":["serve"],"args":[],"resources":{"cpu":1,"memoryBytes":16777216,"pids":64}
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
	if len(workloads) != 1 || workloads[0].msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_PULL {
		t.Fatalf("workloads after prepare = %+v", workloads)
	}
	for _, operation := range []agentv1.WorkloadOp{
		agentv1.WorkloadOp_WORKLOAD_OP_PULL,
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
	     "command":["serve"],"args":[],"resources":{"cpu":1,"memoryBytes":16777216,"pids":64}},
	    {"image":{"reference":"fallback:v1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	     "command":["serve"],"args":[],"resources":{"cpu":1,"memoryBytes":16777216,"pids":64}}
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

func deploymentRow(t *testing.T, h *harness, depID string) db.GetDeploymentRow {
	t.Helper()
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
	if _, err := h.svc.Create(context.Background(), CreateRequest{
		RecipeDigest: "recipe-1", PlanDigest: "sha256:stale",
	}); err == nil {
		t.Fatal("create with stale plan digest: want error")
	}
}

func TestHFOriginFetchIsPreparableAndDispatched(t *testing.T) {
	revision := strings.Repeat("a", 40)
	identity := "hf://Acme/Model@" + revision
	manifest := strings.ReplaceAll(
		artifactManifest,
		`"source": {"type": "local", "identity": "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`"source": {"type": "huggingface", "identity": "hf://Acme/Model", "revision": "`+revision+`"}`,
	)
	h := newHarness(t)
	h.seedNode(t, "dest", gpuAccs("d"), "")
	h.seedArtifact(t, "hf-artifact", identity)
	h.seedRecipe(t, "recipe-origin", manifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{
		RecipeDigest: "recipe-origin", Placements: []PlacementOverride{{NodeID: "dest", Rank: 0}},
	})
	if err != nil || !plan.Ready || len(plan.Transfers) != 1 || plan.Transfers[0].SourceNode != "origin" {
		t.Fatalf("origin plan = %+v, err=%v", plan, err)
	}
	deployment := h.createDeployment(t, "recipe-origin", PlacementOverride{NodeID: "dest", Rank: 0})
	commands := h.nodes.artifactCommands()
	if len(commands) != 1 || commands[0].nodeID != "dest" ||
		commands[0].msg.GetArtifactCommand().GetArtifactIdentity() != identity {
		t.Fatalf("artifact commands = %+v", commands)
	}
	if len(h.nodes.workloadCommands()) != 0 {
		t.Fatal("workload dispatched before origin placement validation")
	}
	command := commands[0].msg.GetArtifactCommand()
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: false, Error: "origin unavailable"})
	row := deploymentRow(t, h, deployment.ID)
	if row.ObservedState != "failed" || !strings.Contains(row.Diagnostics, "artifact.fetch_failed") {
		t.Fatalf("deployment after artifact failure = %+v", row)
	}
}

// TestMissingArtifactGatesThenUnblocks: a rank whose node lacks a valid
// placement is gated; a peer transfer is initiated; the destination's valid
// placement report marks the transfer succeeded and re-drives dispatch.
func TestMissingArtifactGatesThenUnblocks(t *testing.T) {
	const identity = "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := newHarness(t)
	h.seedNode(t, "dest", gpuAccs("d"), "")
	h.seedNode(t, "src", gpuAccs("s"), "100.86.3.45:4433")
	h.seedArtifact(t, "art-1", identity)
	h.seedPlacement(t, "art-1", "src", "/var/lib/lmw/artifacts/model", "valid")
	h.seedRecipe(t, "recipe-art", artifactManifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{
		RecipeDigest: "recipe-art",
		Placements:   []PlacementOverride{{NodeID: "dest", Rank: 0}},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Transfers) != 1 || plan.Transfers[0].SourceNode != "src" || plan.Transfers[0].DestNode != "dest" {
		t.Fatalf("transfer previews = %+v", plan.Transfers)
	}
	dep := h.createDeployment(t, "recipe-art", PlacementOverride{NodeID: "dest", Rank: 0})

	// Gated: no workload command may have gone out before the transfer.
	if wcs := h.nodes.workloadCommands(); len(wcs) != 0 {
		t.Fatalf("workload commands before gate passed: %+v", wcs)
	}
	tcs := h.nodes.transferCommands()
	if len(tcs) != 1 {
		t.Fatalf("transfer commands = %d, want one destination pull command", len(tcs))
	}
	tc := tcs[0].msg.GetTransferCommand()
	tid := tc.GetTransferId()
	if tcs[0].nodeID != "dest" || tc.GetRole() != "dest" || tc.GetPeerAddress() != "100.86.3.45:4433" {
		t.Fatalf("transfer command = node:%s command:%+v", tcs[0].nodeID, tc)
	}
	if state, _ := transferState(t, h, tid); state != "pending" {
		t.Fatalf("transfer state = %s, want pending", state)
	}

	// Destination writes the copy and reports it valid.
	h.seedPlacement(t, "art-1", "dest", "/var/lib/lmw/artifacts/model", "valid")
	h.svc.OnPlacementReport(context.Background(), "dest", identity, "valid")

	if state, _ := transferState(t, h, tid); state != "succeeded" {
		t.Fatalf("transfer state = %s, want succeeded", state)
	}
	wcs := h.nodes.workloadCommands()
	if len(wcs) != 1 || wcs[0].nodeID != "dest" ||
		wcs[0].msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_PULL {
		t.Fatalf("post-gate workload commands = %+v", wcs)
	}
	pull := wcs[0].msg.GetWorkloadCommand()
	h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{
		CommandId: pull.GetCommandId(), Ok: true,
	})

	row := deploymentRow(t, h, dep.ID)
	if got := ParseDispatch(row.Dispatch).Get(0); got != PhasePulled {
		t.Fatalf("rank 0 phase = %s, want %s", got, PhasePulled)
	}
}

// TestInvalidPlacementFailsTransfer: a placement report with state != valid
// marks the in-flight transfer failed and fails the affected rank.
func TestInvalidPlacementFailsTransfer(t *testing.T) {
	const identity = "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := newHarness(t)
	h.seedNode(t, "dest", gpuAccs("d"), "")
	h.seedNode(t, "src", gpuAccs("s"), "100.86.3.45:4433")
	h.seedArtifact(t, "art-1", identity)
	h.seedPlacement(t, "art-1", "src", "/var/lib/lmw/artifacts/model", "valid")
	h.seedRecipe(t, "recipe-art", artifactManifest)

	dep := h.createDeployment(t, "recipe-art", PlacementOverride{NodeID: "dest", Rank: 0})
	tcs := h.nodes.transferCommands()
	if len(tcs) != 1 {
		t.Fatalf("transfer commands = %d, want one destination pull command", len(tcs))
	}
	tid := tcs[0].msg.GetTransferCommand().GetTransferId()

	h.seedPlacement(t, "art-1", "dest", "/var/lib/lmw/artifacts/model", "invalid")
	h.svc.OnPlacementReport(context.Background(), "dest", identity, "invalid")

	state, diagnostic := transferState(t, h, tid)
	if state != "failed" || !strings.Contains(diagnostic, "invalid placement") {
		t.Fatalf("transfer = (%s, %s), want failed/invalid", state, diagnostic)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Failed) {
		t.Fatalf("run state = %s, want %s", got, runs.Failed)
	}
}

// TestFailedTransferAckFailsRank: a failed transfer ack marks the transfer
// row failed and fails the affected rank's run.
func TestFailedTransferAckFailsRank(t *testing.T) {
	const identity = "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := newHarness(t)
	h.seedNode(t, "dest", gpuAccs("d"), "")
	h.seedNode(t, "src", gpuAccs("s"), "100.86.3.45:4433")
	h.seedArtifact(t, "art-1", identity)
	h.seedPlacement(t, "art-1", "src", "/var/lib/lmw/artifacts/model", "valid")
	h.seedRecipe(t, "recipe-art", artifactManifest)

	dep := h.createDeployment(t, "recipe-art", PlacementOverride{NodeID: "dest", Rank: 0})
	tcs := h.nodes.transferCommands()
	if len(tcs) != 1 {
		t.Fatalf("transfer commands = %d, want one destination pull command", len(tcs))
	}
	tid := tcs[0].msg.GetTransferCommand().GetTransferId()

	h.svc.OnTransferResult(context.Background(), tid, "dial: connection refused")

	state, diagnostic := transferState(t, h, tid)
	if state != "failed" || !strings.Contains(diagnostic, "connection refused") {
		t.Fatalf("transfer = (%s, %s), want failed/refused", state, diagnostic)
	}
	if got := runState(t, h, dep.RunID); got != string(runs.Failed) {
		t.Fatalf("run state = %s, want %s", got, runs.Failed)
	}
	row := deploymentRow(t, h, dep.ID)
	var codes []string
	for _, d := range diag.Decode(row.Diagnostics) {
		codes = append(codes, d.Code)
	}
	if !contains(codes, "artifact.transfer_failed") {
		t.Fatalf("deployment diagnostics = %v, want artifact.transfer_failed", codes)
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
		DeploymentId: dep.ID, ContainerId: "c1", State: "missing", Rank: 0,
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
		DeploymentId: dep.ID, ContainerId: "c1", State: "missing", Rank: 0,
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
	if got := ParseDispatch(row.Dispatch).Get(0); got != PhaseNone && got != PhasePrepared {
		t.Fatalf("rank 0 phase after start = %s, want none/prepared", got)
	}
	var n int
	if err := h.dbh.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM leases WHERE resource=? AND state='active'",
		"gpu:node-a:"+acc).Scan(&n); err != nil || n != 1 {
		t.Fatalf("active gpu leases = %d (err %v), want 1", n, err)
	}
	cmds := h.nodes.workloadCommands()
	if last := cmds[len(cmds)-1].msg.GetWorkloadCommand().GetOp(); last != agentv1.WorkloadOp_WORKLOAD_OP_PULL {
		t.Fatalf("last workload op after start = %v, want PULL", last)
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
		DeploymentId: dep.ID, ContainerId: "c1", State: "missing", Rank: 0,
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
