package downloads

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type downloadNodes struct {
	mu          sync.Mutex
	online      bool
	service     *Service
	commands    []*agentv1.DownloadCommand
	available   bool
	holdFetch   bool
	inspectPath func(string) ResourceState
}

func (n *downloadNodes) Online(string) bool { n.mu.Lock(); defer n.mu.Unlock(); return n.online }
func (n *downloadNodes) Send(node string, m *agentv1.ServerMessage) bool {
	cmd := m.GetDownloadCommand()
	if cmd == nil {
		panic("download sent a workload or undeclared command")
	}
	n.mu.Lock()
	if !n.online {
		n.mu.Unlock()
		return false
	}
	n.commands = append(n.commands, cmd)
	available, hold, inspectPath := n.available, n.holdFetch, n.inspectPath
	if cmd.Op == agentv1.DownloadOp_DOWNLOAD_OP_FETCH && !hold {
		n.available = true
		available = true
	}
	n.mu.Unlock()
	if cmd.Op == agentv1.DownloadOp_DOWNLOAD_OP_FETCH && hold {
		return true
	}
	var spec ResourceSpec
	if json.Unmarshal(cmd.ResourceJson, &spec) != nil {
		panic("invalid resource")
	}
	now := time.Now().UTC()
	size, free := int64(8), StorageReserveBytes+1024
	state := ResourceMissing
	if available {
		state = ResourceAvailable
	}
	if inspectPath != nil && cmd.Op == agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
		state = inspectPath(spec.Destination)
	}
	out := CommandOutput{ItemID: cmd.ItemId, Identity: spec.Identity, Path: spec.Destination, Platform: spec.Platform, State: state, SizeBytes: &size, Storage: &StorageObservation{Filesystem: "fs-cache", Destination: spec.Destination, AvailableBytes: &free, TotalBytes: &free}}
	if state == ResourceAvailable {
		out.VerifiedAt = &now
		if spec.Kind == ResourceImage {
			out.IndexDigest = spec.IndexDigest
			out.ManifestDigest = spec.ManifestDigest
			if out.ManifestDigest == "" {
				out.ManifestDigest = spec.IndexDigest
			}
		} else {
			out.TreeDigest = "sha256:" + strings.Repeat("d", 64)
			out.TreeSizeBytes = &size
		}
	}
	n.service.OnResult(context.Background(), node, &agentv1.CommandResult{CommandId: cmd.CommandId, Ok: true, OutputJson: []byte(encoded(out))})
	return true
}
func downloadHarness(t *testing.T) (*Service, *downloadNodes) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "downloads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	q := db.New(database)
	bus := events.NewEventBus(q)
	ledger := runs.New(database, q, bus, t.TempDir())
	nodes := &downloadNodes{online: true}
	service := New(database, q, ledger, nodes, nil, nil, nil, nil)
	nodes.service = service
	for _, node := range []string{"node-a", "node-b"} {
		if err = q.CreateNode(ctx, db.CreateNodeParams{ID: node, DisplayName: node, Labels: "{}"}); err != nil {
			t.Fatal(err)
		}
		if err = q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: node, Inventory: ns(`{"protocol_features":["downloads-v1"]}`)}); err != nil {
			t.Fatal(err)
		}
	}
	return service, nodes
}
func testResource() Resource {
	digest := "sha256:" + strings.Repeat("a", 64)
	r := Resource{ResourceSpec: ResourceSpec{Kind: ResourceArtifact, Identity: "file://" + digest, Source: SourceSpec{Type: SourceFile, Digest: digest, URL: "https://example.com/model.bin"}, Destination: "/cache/files/" + strings.Repeat("a", 64)}, NodeID: "node-a", Action: ActionDownloadOrigin, Required: true}
	r.Key = resourceKey(r)
	return r
}
func seedDownloadItem(t *testing.T, s *Service, r Resource) (string, db.DownloadItem) {
	t.Helper()
	ctx := context.Background()
	plan := Plan{RecipeDigest: "sha256:" + strings.Repeat("b", 64), WorkloadIndex: 0, Variants: map[string]string{}, Targets: []Target{{NodeID: r.NodeID, CacheRoot: "/cache"}}, Resources: []Resource{r}, Ready: true}
	plan.PlanDigest = planHash(&plan)
	input := AttemptInput{Plan: plan}
	var body map[string]any
	_ = json.Unmarshal([]byte(encoded(input)), &body)
	runID, err := s.runs.Create(ctx, "library", JobKind, body, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.runs.SetState(ctx, runID, runs.Running, "", ""); err != nil {
		t.Fatal(err)
	}
	itemID := newID()
	if err = s.q.CreateDownloadItem(ctx, db.CreateDownloadItemParams{ID: itemID, RunID: runID, NodeID: r.NodeID, ResourceKey: r.Key, ResourceJson: encoded(r)}); err != nil {
		t.Fatal(err)
	}
	row, err := s.q.GetDownloadItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return runID, row
}

func TestAcquisitionVerifiesFilesWithoutDeploymentOrWorkloadCommands(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	r := testResource()
	runID, row := seedDownloadItem(t, s, r)
	if err := s.acquireItem(ctx, row, nil); err != nil {
		t.Fatal(err)
	}
	items, err := s.q.ListDownloadItems(ctx, runID)
	if err != nil || len(items) != 1 || items[0].State != "succeeded" {
		t.Fatalf("items=%v error=%v", items, err)
	}
	if len(n.commands) != 3 || n.commands[0].Op != agentv1.DownloadOp_DOWNLOAD_OP_INSPECT || n.commands[1].Op != agentv1.DownloadOp_DOWNLOAD_OP_FETCH || n.commands[2].Op != agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
		t.Fatalf("expected inspect/fetch/final inspect: %v", n.commands)
	}
	for _, table := range []string{"deployments", "leases"} {
		var count int
		if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("download mutated %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestOfflineCancellationRetainsWriterUntilEnrolledAcknowledgement(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	r := testResource()
	runID, row := seedDownloadItem(t, s, r)
	if err := s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerDownload, row.ID, runID, row.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.transition(ctx, row, ItemChecking, nil)
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	command := newID()
	_, err := s.q.BindDownloadItemCommand(ctx, db.BindDownloadItemCommandParams{ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State, CommandID: ns(command)})
	if err != nil {
		t.Fatal(err)
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	_, _ = s.transition(ctx, row, ItemTransferring, nil)
	n.online = false
	if err = s.Cancel(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(ctx, runID); err != nil {
		t.Fatalf("repeated cancellation must continue waiting for quiescence: %v", err)
	}
	run, _ := s.runs.Get(ctx, runID)
	if run.State != "cancelling" {
		t.Fatalf("offline cancel prematurely terminal: %s", run.State)
	}
	if err = s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerTransfer, "competing", "", ""); err == nil {
		t.Fatal("offline cancellation released writer")
	}
	out := CommandOutput{ItemID: row.ID, Identity: r.Identity, Path: r.Destination, State: ResourceMissing}
	if !s.OnResult(ctx, "node-b", &agentv1.CommandResult{CommandId: command, Ok: true, OutputJson: []byte(encoded(out))}) {
		t.Fatal("wrong-node owned result escaped owner routing")
	}
	if err = s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerTransfer, "competing", "", ""); err == nil {
		t.Fatal("wrong node released writer")
	}
	s.OnResult(ctx, r.NodeID, &agentv1.CommandResult{CommandId: command, Ok: true, OutputJson: []byte(encoded(out))})
	run, _ = s.runs.Get(ctx, runID)
	if run.State != "cancelled" {
		t.Fatalf("matching quiescence did not finish cancellation: %s", run.State)
	}
	if err = s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerTransfer, "competing", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPeerCancellationBeforeTransferBindingReleasesWriter(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	r := testResource()
	r.Action = ActionPeerCopy
	runID, row := seedDownloadItem(t, s, r)
	if err := s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerDownload, row.ID, runID, row.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.transition(ctx, row, ItemChecking, nil)
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	// Older attempts allocated an origin command before preparing a peer copy.
	if _, err := s.q.BindDownloadItemCommand(ctx, db.BindDownloadItemCommandParams{ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State, CommandID: ns(newID())}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	_, _ = s.transition(ctx, row, ItemTransferring, nil)
	n.online = false
	if err := s.Cancel(ctx, runID); err != nil {
		t.Fatal(err)
	}
	run, _ := s.runs.Get(ctx, runID)
	if run.State != "cancelled" {
		t.Fatalf("undispatched peer copy remained active: %s", run.State)
	}
	if err := s.Lock(ctx, r.NodeID, r.Destination, r.Identity, "", OwnerTransfer, "next-owner", "", ""); err != nil {
		t.Fatal("undispatched peer copy retained writer ownership:", err)
	}
}

func TestTerminalProgressCannotResurrectDownload(t *testing.T) {
	s, _ := downloadHarness(t)
	ctx := context.Background()
	r := testResource()
	_, row := seedDownloadItem(t, s, r)
	if err := s.acquireItem(ctx, row, nil); err != nil {
		t.Fatal(err)
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	before := row.CheckpointJson
	s.OnProgress(ctx, row.NodeID, &agentv1.DownloadProgress{ItemId: row.ID, CommandId: row.CommandID.String, Phase: "transferring", BytesDone: 7})
	after, _ := s.q.GetDownloadItem(ctx, row.ID)
	if after.State != "succeeded" || after.CheckpointJson != before {
		t.Fatalf("late progress mutated terminal item: %+v", after)
	}
}

func TestAnyVerifiedExactLocalPathCanSatisfyResource(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	r := testResource()
	if err := s.q.CreateArtifact(ctx, db.CreateArtifactParams{ID: "artifact-a", Kind: "file", Identity: r.Identity, Metadata: "{}"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{r.Destination, "/cache/verified-existing"} {
		if err := s.q.UpsertPlacement(ctx, db.UpsertPlacementParams{ArtifactID: "artifact-a", NodeID: r.NodeID, Path: p, State: "invalid", Diagnostics: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	n.inspectPath = func(p string) ResourceState {
		if p == "/cache/verified-existing" {
			return ResourceAvailable
		}
		return ResourceInvalid
	}
	out, err := s.inspectCandidates(ctx, &r, Target{NodeID: r.NodeID, CacheRoot: "/cache"}, nil)
	if err != nil || out.State != ResourceAvailable || r.Destination != "/cache/verified-existing" {
		t.Fatalf("later exact verified placement not reused: %+v %+v %v", r, out, err)
	}
}

func TestPlanDigestBindsSourceButNotProgressOrCapacity(t *testing.T) {
	r := testResource()
	p := Plan{RecipeDigest: "recipe", Resources: []Resource{r}, Targets: []Target{{NodeID: r.NodeID, CacheRoot: "/cache"}}, Variants: map[string]string{}}
	before := planHash(&p)
	size := int64(500)
	now := time.Now()
	p.Resources[0].BytesRemaining = &size
	p.Resources[0].Verification.VerifiedAt = &now
	p.Storage = []Storage{{AvailableBytes: &size}}
	if planHash(&p) != before {
		t.Fatal("observation changed reviewed identities")
	}
	p.Resources[0].Action = ActionPeerCopy
	p.Resources[0].SourceNode = "node-b"
	if planHash(&p) == before {
		t.Fatal("changed network source did not require review")
	}
}

func TestUnknownStorageAndExpandedImageSizeRemainBlockers(t *testing.T) {
	r := testResource()
	r.Kind = ResourceImage
	r.BytesRemaining = nil
	free := StorageReserveBytes + 1000
	storage := storagePlan([]Resource{r}, map[string]*StorageObservation{r.Key: {Filesystem: "engine", AvailableBytes: &free}})
	if len(storage) != 1 || storage[0].Sufficient != nil || storage[0].RequiredBytes != nil {
		t.Fatalf("unknown expanded image storage admitted: %+v", storage)
	}
	bytes := int64(800)
	r.BytesRemaining = &bytes
	storage = storagePlan([]Resource{r}, map[string]*StorageObservation{r.Key: {Filesystem: "engine", AvailableBytes: &free}})
	if storage[0].Sufficient == nil || *storage[0].Sufficient {
		t.Fatal("storage ignored simultaneous staging and final publication")
	}
}

func TestPlanPrefersVerifiedReuseThenPartialOriginThenDeterministicPeerWithoutGPUGates(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	resource := testResource()
	digest := "sha256:" + strings.Repeat("b", 64)
	imageDigest := "sha256:" + strings.Repeat("c", 64)
	image := recipe.Image{Reference: "docker.io/library/fixture", Digest: imageDigest}
	manifest := recipe.Manifest{
		Artifacts: []recipe.Artifact{{Name: "weights", Kind: "model", SizeBytes: 8, Source: &recipe.ArtSource{Type: "file", Identity: resource.Source.URL, Digest: resource.Source.Digest}}},
		Workloads: []recipe.Workload{{Image: image, Match: &recipe.Match{Accelerator: &recipe.MatchAcc{Vendor: "nvidia", Architectures: []string{"unavailable-architecture"}}}}},
		Prepare:   &recipe.Extension{Image: image}, Verify: &recipe.Extension{Image: image},
	}
	if err := s.q.CreateRecipe(ctx, db.CreateRecipeParams{Digest: digest, Name: "fixture", Version: "1", Source: "{}", Manifest: encoded(manifest)}); err != nil {
		t.Fatal(err)
	}
	inventory := `{"protocol_features":["downloads-v1"],"cache_roots":[{"path":"/cache","writable":true}],"peer_listen":"node-b.test:9444","download_roots":{"recipe_root":"/recipes","image_root":"/engine","platform":"linux/amd64"}}`
	for _, node := range []string{"node-a", "node-b"} {
		if err := s.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: node, Inventory: ns(inventory)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.q.CreateArtifact(ctx, db.CreateArtifactParams{ID: "model", Kind: "file", Identity: resource.Identity, Metadata: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.q.UpsertPlacement(ctx, db.UpsertPlacementParams{ArtifactID: "model", NodeID: "node-b", Path: "/cache/peer-existing", State: "valid", Diagnostics: "[]", SizeBytes: 8}); err != nil {
		t.Fatal(err)
	}
	state := ResourcePartial
	n.inspectPath = func(destination string) ResourceState {
		if destination == resource.Destination {
			return state
		}
		return ResourceAvailable
	}
	request := PlanRequest{RecipeDigest: digest, Targets: []Target{{NodeID: "node-a"}}}
	partial, err := s.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Ready || len(partial.Resources) != 3 {
		t.Fatalf("storage-only plan should deduplicate helper images and ignore incompatible GPU: %+v", partial)
	}
	find := func(p *Plan) Resource {
		for _, r := range p.Resources {
			if r.Identity == resource.Identity {
				return r
			}
		}
		t.Fatal("selected immutable artifact missing")
		return Resource{}
	}
	if got := find(partial); got.Action != ActionDownloadOrigin || got.SourceNode != "" {
		t.Fatalf("verified partial did not retain origin resume: %+v", got)
	}
	state = ResourceMissing
	peer, err := s.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(peer); got.Action != ActionPeerCopy || got.SourceNode != "node-b" {
		t.Fatalf("missing artifact did not select exact verified peer: %+v", got)
	}
	if peer.PlanDigest == partial.PlanDigest {
		t.Fatal("network action change did not invalidate reviewed plan")
	}
	state = ResourceAvailable
	reused, err := s.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(reused); got.Action != ActionReuse || got.SourceNode != "" {
		t.Fatalf("exact local bytes did not win over peer/origin: %+v", got)
	}
	state = ResourceMissing
	if err := s.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: "node-b", Inventory: ns(strings.ReplaceAll(inventory, "node-b.test:9444", "[::]:9444"))}); err != nil {
		t.Fatal(err)
	}
	origin, err := s.Plan(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(origin); !origin.Ready || got.Action != ActionDownloadOrigin || got.SourceNode != "" {
		t.Fatalf("unusable peer address blocked a retrievable origin: %+v", origin)
	}
	for _, command := range n.commands {
		if command.Op != agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
			t.Fatal("opening a plan started acquisition")
		}
	}
}

func TestControllerRestartRetainsLocksAndReconnectOnlyCancels(t *testing.T) {
	s, n := downloadHarness(t)
	ctx := context.Background()
	resource := testResource()
	runID, row := seedDownloadItem(t, s, resource)
	if err := s.Lock(ctx, row.NodeID, resource.Destination, resource.Identity, "", OwnerDownload, row.ID, runID, row.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = s.transition(ctx, row, ItemChecking, nil)
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	command := newID()
	if _, err := s.q.BindDownloadItemCommand(ctx, db.BindDownloadItemCommandParams{ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State, CommandID: ns(command)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	interrupted, _ := s.q.GetDownloadItem(ctx, row.ID)
	if interrupted.State != "interrupted" {
		t.Fatalf("restart did not interrupt: %+v", interrupted)
	}
	if err := s.Lock(ctx, row.NodeID, resource.Destination, resource.Identity, "", OwnerTransfer, "other", "", ""); err == nil {
		t.Fatal("restart inferred device quiescence")
	}
	s.OnReconnect(ctx, row.NodeID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		lockErr := s.Lock(ctx, row.NodeID, resource.Destination, resource.Identity, "", OwnerTransfer, "other", "", "")
		if lockErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("matching cancellation did not release interrupted writer")
		}
		time.Sleep(time.Millisecond)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.commands) != 1 || n.commands[0].Op != agentv1.DownloadOp_DOWNLOAD_OP_CANCEL || n.commands[0].TargetCommandId != command {
		t.Fatalf("reconnect replayed acquisition rather than cancelling: %+v", n.commands)
	}
	after, _ := s.q.GetDownloadItem(ctx, row.ID)
	if after.State != "interrupted" {
		t.Fatal("reconnect revived terminal attempt")
	}
}

func TestImageDiscoveryCannotAuthorizeFetchWithoutPlatformManifest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	spec := ResourceSpec{Kind: ResourceImage, Identity: "docker.io/library/fixture@" + digest, Source: SourceSpec{Type: SourceOCI, Reference: "docker.io/library/fixture", Digest: digest}, Destination: "/engine", Platform: "linux/amd64", IndexDigest: digest}
	raw := []byte(encoded(spec))
	if _, err := DecodeInspectionSpec(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeResourceSpec(raw); err == nil {
		t.Fatal("read-only index discovery authorized an unbound image fetch")
	}
	now := time.Now().UTC()
	out := CommandOutput{ItemID: "item", Identity: spec.Identity, Path: spec.Destination, Platform: spec.Platform, State: ResourceAvailable, VerifiedAt: &now, IndexDigest: digest, ManifestDigest: "sha256:" + strings.Repeat("f", 64)}
	if err := validateOutput(out, "item", spec); err != nil {
		t.Fatal(err)
	}
	out.IndexDigest = "sha256:" + strings.Repeat("a", 64)
	if err := validateOutput(out, "item", spec); err == nil {
		t.Fatal("native image discovery substituted a different index")
	}
}

func TestImagePlatformTreatsOnlyARM64V8AsBaseline(t *testing.T) {
	baseline := CanonicalImagePlatform("linux", "arm64", "")
	if CanonicalImagePlatform("linux", "arm64", "v8") != baseline {
		t.Fatal("baseline ARM64 rejected an explicitly marked v8 image")
	}
	if CanonicalImagePlatform("linux", "arm64", "v9") == baseline {
		t.Fatal("a newer ISA requirement was discarded")
	}
}
