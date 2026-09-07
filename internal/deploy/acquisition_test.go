package deploy

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type fakeAcquisition struct {
	service    *Service
	plan       *downloads.Plan
	acquireErr error
	acquire    func(context.Context, downloads.PlanRequest, bool) error
	prepareErr error
}

func (f *fakeAcquisition) SupportsNode(context.Context, string) bool { return true }

func (f *fakeAcquisition) PreparePeer(_ context.Context, req downloads.PeerRequest) (*downloads.PreparedPeer, error) {
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	destination := "/cache/files/" + strings.TrimPrefix(req.Identity, "file://sha256:")
	if req.Destination != "" && req.Destination != destination {
		return nil, errors.New("noncanonical destination")
	}
	return &downloads.PreparedPeer{Command: &agentv1.TransferCommand{TransferId: req.TransferID, Op: agentv1.TransferOp_TRANSFER_OP_START, Role: "dest", PeerAddress: req.PeerAddress, ArtifactIdentity: req.Identity, SrcPath: req.SourcePath, DestPath: destination}}, nil
}

func (f *fakeAcquisition) Plan(ctx context.Context, req downloads.PlanRequest) (*downloads.Plan, error) {
	if f.plan != nil {
		return f.plan, nil
	}
	plan := &downloads.Plan{RecipeDigest: req.RecipeDigest, WorkloadIndex: *req.WorkloadIndex, Variants: req.Variants, Targets: req.Targets, Ready: true}
	if f.service == nil {
		return plan, nil
	}
	manifest, err := f.service.manifestFor(ctx, req.RecipeDigest)
	if err != nil {
		return nil, err
	}
	workload := manifest.Workloads[*req.WorkloadIndex]
	images := []recipe.Image{workload.Image}
	if manifest.Prepare != nil {
		images = append(images, manifest.Prepare.Image)
	}
	if manifest.Verify != nil {
		images = append(images, manifest.Verify.Image)
	}
	now := time.Now()
	for _, target := range req.Targets {
		resources := []downloads.ResourceSpec{{Kind: downloads.ResourceRecipe, Identity: "recipe://" + req.RecipeDigest, Destination: "/var/lib/lmw/recipes/" + req.RecipeDigest}}
		for _, artifact := range manifest.Artifacts {
			identity, err := canonicalArtifactIdentity(artifact, req.Variants[artifact.Name])
			if err != nil {
				return nil, err
			}
			destination := "/cache/" + artifact.Name
			if row, err := f.service.q.GetArtifactByIdentity(ctx, identity); err == nil {
				if path, valid := f.service.validPlacement(ctx, row.ID, target.NodeID); valid {
					destination = path
				}
			}
			resources = append(resources, downloads.ResourceSpec{Kind: downloads.ResourceArtifact, Identity: identity, Destination: destination})
		}
		for _, image := range images {
			resources = append(resources, downloads.ResourceSpec{Kind: downloads.ResourceImage, Identity: "oci://" + image.Reference + "@" + image.Digest, Source: downloads.SourceSpec{Reference: image.Reference}, IndexDigest: image.Digest, ManifestDigest: image.Digest, Platform: "linux/amd64", Destination: "engine"})
		}
		for _, resource := range resources {
			plan.Resources = append(plan.Resources, downloads.Resource{ResourceSpec: resource, Key: target.NodeID + "|" + resource.Identity, NodeID: target.NodeID, Action: downloads.ActionReuse, Required: true, Verification: downloads.Verification{State: downloads.ResourceAvailable, VerifiedAt: &now}})
		}
	}
	return plan, nil
}

func (f *fakeAcquisition) Acquire(ctx context.Context, req downloads.PlanRequest, requireExisting bool) error {
	if f.acquire != nil {
		return f.acquire(ctx, req, requireExisting)
	}
	return f.acquireErr
}

func (f *fakeAcquisition) Lock(ctx context.Context, node, destination, identity, platform string, kind downloads.OwnerKind, owner, run, item string) error {
	return f.service.q.AcquireDestinationLock(ctx, db.AcquireDestinationLockParams{NodeID: node, Destination: destination, Identity: identity, Platform: platform, OwnerKind: string(kind), OwnerID: owner, RunID: nullString(run), ItemID: nullString(item)})
}
func (f *fakeAcquisition) MarkCancelling(ctx context.Context, node, destination string, kind downloads.OwnerKind, owner string) error {
	_, err := f.service.q.CancelDestinationLock(ctx, db.CancelDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner})
	return err
}
func (f *fakeAcquisition) Unlock(ctx context.Context, node, destination string, kind downloads.OwnerKind, owner string, proof downloads.QuiescenceProof) error {
	lock, err := f.service.q.GetDestinationLock(ctx, db.GetDestinationLockParams{NodeID: node, Destination: destination})
	if err != nil {
		return err
	}
	_, err = f.service.q.QuiesceDestinationLock(ctx, db.QuiesceDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner, ExpectedState: lock.State, QuiescenceProof: nullString(string(proof))})
	if err != nil {
		return err
	}
	_, err = f.service.q.ReleaseDestinationLock(ctx, db.ReleaseDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner})
	return err
}

func waitAcquisition(t *testing.T, service *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		service.mu.Lock()
		active := len(service.acquisitionLive)
		service.mu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("serving acquisition gate did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) createReviewed(ctx context.Context, req CreateRequest) (*Deployment, error) {
	if req.PlanDigest == "" {
		plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: req.RecipeDigest, Placements: req.Placements, LaunchProfileID: req.LaunchProfileID, Parameters: req.Parameters, Variants: req.Variants, WorkloadIndex: req.WorkloadIndex, AcquisitionPolicy: req.AcquisitionPolicy})
		if err != nil {
			return nil, err
		}
		req.PlanDigest = plan.Digest
	}
	return h.svc.Create(ctx, req)
}

func TestCreateRequiresReviewAndExistingResourcesBeforeCommit(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe", noArtifactManifest)
	if _, err := h.svc.Create(context.Background(), CreateRequest{RecipeDigest: "recipe"}); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("unreviewed creation = %v", err)
	}
	h.svc.downloads = &fakeAcquisition{acquireErr: errors.New("exact image is missing")}
	if _, err := h.createReviewed(context.Background(), CreateRequest{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionRequireExisting}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("missing exact image creation = %v", err)
	}
	rows, err := h.q.ListDeployments(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed precommit persisted deployments: %+v, %v", rows, err)
	}
	if len(h.nodes.workloadCommands()) != 0 || len(h.nodes.artifactCommands()) != 0 || len(h.nodes.transferCommands()) != 0 || len(h.nodes.extensionCommands()) != 0 {
		t.Fatal("require-existing failure dispatched acquisition or execution")
	}
}

func TestRequireExistingRechecksEveryLaunchGate(t *testing.T) {
	for _, phase := range []string{PhaseNone, PhasePreparing, PhasePrepared, PhasePulled, PhaseCreated, PhaseHostPreparing, PhaseHostPrepared, PhaseVerifying} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t)
			h.seedNode(t, "node", nil, "")
			h.seedRecipe(t, "recipe", noArtifactManifest)
			dep := h.createDeployment(t, "recipe")
			h.nodes.mu.Lock()
			h.nodes.msgs = nil
			h.nodes.mu.Unlock()
			h.svc.downloads = &fakeAcquisition{acquireErr: errors.New("previously available package was removed")}
			h.svc.setPhase(context.Background(), dep.ID, 0, phase)
			h.svc.dispatchNext(context.Background(), dep.ID, 0, dep.RunID, h.svc.placementFor(context.Background(), dep.ID, 0))
			waitAcquisition(t, h.svc)
			for _, sent := range h.nodes.workloadCommands() {
				if sent.msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
					t.Fatalf("failed gate launched operation: %+v", sent.msg)
				}
			}
			for _, sent := range h.nodes.extensionCommands() {
				if sent.msg.GetExtensionCommand().GetPhase() != "stop" {
					t.Fatalf("failed gate executed extension: %+v", sent.msg)
				}
			}
			if len(h.nodes.artifactCommands()) != 0 || len(h.nodes.transferCommands()) != 0 {
				t.Fatal("failed require-existing gate acquired bytes")
			}
			row := deploymentRow(t, h, dep.ID)
			if row.DesiredState == "running" {
				t.Fatal("missing resources left deployment authorized to run")
			}
		})
	}
}

func TestDownloadMissingCompletesAcquisitionBeforeCreateWithoutPull(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe", noArtifactManifest)
	var acquired atomic.Bool
	h.svc.downloads = &fakeAcquisition{service: h.svc, acquire: func(_ context.Context, _ downloads.PlanRequest, requireExisting bool) error {
		if requireExisting {
			return errors.New("download-missing incorrectly required existing resources")
		}
		acquired.Store(true)
		return nil
	}}
	h.nodes.onSend = func(message *agentv1.ServerMessage) {
		if message.GetWorkloadCommand() != nil && !acquired.Load() {
			t.Error("workload dispatched before acquisition completed")
		}
	}
	dep, err := h.createReviewed(context.Background(), CreateRequest{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionDownloadMissing})
	if err != nil {
		t.Fatal(err)
	}
	commands := h.nodes.workloadCommands()
	if len(commands) != 1 || commands[0].msg.GetWorkloadCommand().GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
		t.Fatalf("post-acquisition operations = %+v", commands)
	}
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(commands[0].msg.GetWorkloadCommand().GetContainerSpec(), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.AcquisitionPolicy != AcquisitionRequireExisting {
		t.Fatal("runtime may implicitly pull after shared acquisition")
	}
	if ParsePlacementSet(deploymentRow(t, h, dep.ID).Placement).AcquisitionPolicy != AcquisitionDownloadMissing {
		t.Fatal("download policy was not persisted")
	}
}

func TestStopDuringAcquisitionCannotLaunchAfterCompletion(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe", noArtifactManifest)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	h.svc.downloads = &fakeAcquisition{acquire: func(ctx context.Context, _ downloads.PlanRequest, _ bool) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	dep, err := h.createReviewed(context.Background(), CreateRequest{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionDownloadMissing})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("acquisition did not begin")
	}
	_, err = h.svc.Stop(context.Background(), dep.ID)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	waitAcquisition(t, h.svc)
	if len(h.nodes.workloadCommands()) != 0 || len(h.nodes.extensionCommands()) != 0 {
		t.Fatal("completed acquisition launched a stopped deployment")
	}
}

func TestAcquisitionPolicyAndImmutableResourcesBindReview(t *testing.T) {
	plan := &Plan{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionRequireExisting, Acquisition: &downloads.Plan{Targets: []downloads.Target{{NodeID: "node", CacheRoot: "/cache"}}, Resources: []downloads.Resource{{ResourceSpec: downloads.ResourceSpec{Kind: downloads.ResourceImage, Identity: "oci://engine@sha256:one", Destination: "engine", Platform: "linux/amd64"}}}}}
	reviewed := plan.PlanDigest()
	plan.Acquisition.Resources[0].Verification.State = downloads.ResourceAvailable
	if plan.PlanDigest() != reviewed {
		t.Fatal("observation changed the reviewed launch contract")
	}
	plan.Acquisition.Resources[0].Platform = "linux/arm64"
	if plan.PlanDigest() == reviewed {
		t.Fatal("image platform changed without invalidating review")
	}
	plan.Acquisition.Resources[0].Platform = "linux/amd64"
	plan.Acquisition.Resources[0].Action = downloads.ActionDownloadOrigin
	if plan.PlanDigest() == reviewed {
		t.Fatal("network acquisition changed without invalidating review")
	}
	plan.Acquisition.Resources[0].Action = ""
	plan.Acquisition.Resources[0].SourceNode = "another-peer"
	if plan.PlanDigest() == reviewed {
		t.Fatal("peer source changed without invalidating review")
	}
	plan.Acquisition.Resources[0].SourceNode = ""
	plan.AcquisitionPolicy = AcquisitionDownloadMissing
	if plan.PlanDigest() == reviewed {
		t.Fatal("permission to download changed without invalidating review")
	}
}

func TestPlanExposesMissingResourcesAndRequireExistingBlocks(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe", noArtifactManifest)
	acquisition, err := h.svc.downloads.Plan(context.Background(), downloads.PlanRequest{RecipeDigest: "recipe", Targets: []downloads.Target{{NodeID: "node"}}, WorkloadIndex: new(int)})
	if err != nil {
		t.Fatal(err)
	}
	acquisition.Resources[0].Verification.State = downloads.ResourceMissing
	h.svc.downloads = &fakeAcquisition{service: h.svc, plan: acquisition}
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionRequireExisting})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || len(plan.MissingResources) != 1 || plan.MissingResources[0].Identity != "recipe://recipe" {
		t.Fatalf("require-existing preview hid missing package: %+v", plan)
	}
	downloadPlan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionDownloadMissing})
	if err != nil {
		t.Fatal(err)
	}
	if !downloadPlan.Ready || downloadPlan.Digest == plan.Digest {
		t.Fatal("explicit acquisition permission did not change the reviewed launch contract")
	}
}

func TestReviewedMountDoesNotSelectEarlierInvalidPlacement(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "node", nil, "")
	h.seedRecipe(t, "recipe", artifactManifest)
	h.seedArtifact(t, "model", "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	h.seedPlacement(t, "model", "node", "/cache/aaa-invalid", "invalid")
	h.seedPlacement(t, "model", "node", "/cache/reviewed-model", "valid")
	dep := h.createDeployment(t, "recipe")
	spec, err := h.svc.renderSpec(context.Background(), dep.ID, 0, dep.RunID, &dep.Placements[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range spec.Mounts {
		if mount.Dest == "/var/lib/lmw/artifacts/model" {
			if mount.Source != "/cache/reviewed-model" {
				t.Fatalf("mounted unreviewed path %s", mount.Source)
			}
			return
		}
	}
	t.Fatal("reviewed model mount is missing")
}

func TestTransferCancellationRetainsDestinationUntilAgentBarrier(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "source", nil, "100.86.3.45:9444")
	h.seedNode(t, "dest", nil, "")
	h.seedArtifact(t, "model", "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	h.seedPlacement(t, "model", "source", "/cache/model", "valid")
	artifact, err := h.q.GetArtifact(context.Background(), "model")
	if err != nil {
		t.Fatal(err)
	}
	transferID, err := h.svc.StartTransfer(context.Background(), artifact, "source", "dest", "")
	if err != nil {
		t.Fatal(err)
	}
	h.nodes.setOnline("dest", false)
	if err := h.svc.CancelTransfer(context.Background(), transferID); err != nil {
		t.Fatal(err)
	}
	if state, _ := transferState(t, h, transferID); state != "cancelling" {
		t.Fatalf("offline cancellation became %s", state)
	}
	if _, err := h.q.GetDestinationLock(context.Background(), db.GetDestinationLockParams{NodeID: "dest", Destination: "/cache/files/" + strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("offline cancellation released ownership: %v", err)
	}
	h.nodes.setOnline("dest", true)
	if _, err := h.svc.StartTransfer(context.Background(), artifact, "source", "dest", ""); err == nil {
		t.Fatal("new writer overlapped cancelling transfer")
	}
	if err := h.svc.CancelTransfer(context.Background(), transferID); err != nil {
		t.Fatal(err)
	}
	commands := h.nodes.transferCommands()
	cancel := commands[len(commands)-1].msg.GetTransferCommand()
	if cancel.Op != agentv1.TransferOp_TRANSFER_OP_CANCEL || cancel.TargetTransferId != transferID {
		t.Fatalf("cancellation did not target original writer: %+v", cancel)
	}
	h.svc.OnTransferCommandResult(context.Background(), "dest", &agentv1.CommandResult{CommandId: cancel.TransferId, Ok: true})
	if state, _ := transferState(t, h, transferID); state != "cancelled" {
		t.Fatalf("confirmed cancellation state = %s", state)
	}
	if _, err := h.svc.StartTransfer(context.Background(), artifact, "source", "dest", ""); err != nil {
		t.Fatalf("confirmed cancellation retained destination lock: %v", err)
	}
}

func TestStandaloneTransferRefusesUnverifiedSourceWithoutDispatch(t *testing.T) {
	h := newHarness(t)
	h.seedNode(t, "source", nil, "100.86.3.45:9444")
	h.seedNode(t, "dest", nil, "")
	h.seedArtifact(t, "model", "file://sha256:"+strings.Repeat("a", 64))
	h.seedPlacement(t, "model", "source", "/cache/model", "valid")
	h.svc.downloads = &fakeAcquisition{service: h.svc, prepareErr: errors.New("source tree no longer verifies")}
	artifact, err := h.q.GetArtifact(context.Background(), "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.StartTransfer(context.Background(), artifact, "source", "dest", ""); err == nil {
		t.Fatal("unverified source was dispatched")
	}
	if len(h.nodes.transferCommands()) != 0 {
		t.Fatal("source proof failure sent a transfer command")
	}
	if _, err := h.q.GetDestinationLock(context.Background(), db.GetDestinationLockParams{NodeID: "dest", Destination: "/cache/files/" + strings.Repeat("a", 64)}); err == nil {
		t.Fatal("failed preparation retained write ownership")
	}
}
