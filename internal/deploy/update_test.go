package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestRepositoryUpdatePreservesHardwareAndCompletesOnHealthy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("1", 64)
	newDigest := "sha256:" + strings.Repeat("2", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/recipe\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("a", 40), true)

	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	candidateManifest, err := recipe.Parse([]byte(noArtifactManifest))
	if err != nil {
		t.Fatal(err)
	}
	candidateManifest.Metadata.Name = "test"
	candidateManifest.Workloads[0].Permissions = []string{"rootfs.write"}
	candidateDoc, err := json.Marshal(candidateManifest)
	if err != nil {
		t.Fatal(err)
	}
	h.seedRecipeUnplaced(t, newDigest, string(candidateDoc))
	seedRepositoryVersion(t, h, repositoryID, newDigest, strings.Repeat("b", 40), true)
	request := RepositoryReplacementRequest{TargetDigest: newDigest, DeploymentIDs: []string{source.ID}}
	updatePlan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(updatePlan.CurrentPermissions) != 0 ||
		len(updatePlan.CandidatePermissions) != 1 || updatePlan.CandidatePermissions[0] != "rootfs.write" ||
		len(updatePlan.AddedPermissions) != 1 || updatePlan.AddedPermissions[0] != "rootfs.write" ||
		len(updatePlan.RemovedPermissions) != 0 {
		t.Fatalf("running source permission diff = %+v", updatePlan)
	}
	if !updatePlan.Ready || len(updatePlan.InstalledDevices) != 1 || len(updatePlan.RunningDeployments) != 1 {
		t.Fatalf("update plan = %+v", updatePlan)
	}
	if target := updatePlan.RunningDeployments[0]; target.NodeID != "node-a" || target.SourceDeploymentID != source.ID || target.Rank != 0 {
		t.Fatalf("running deployment target = %+v", target)
	}
	if _, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, "sha256:stale"); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale plan error = %v", err)
	}
	if row := deploymentRow(t, h, source.ID); row.DesiredState != "running" {
		t.Fatalf("stale plan stopped source: %+v", row)
	}

	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, updatePlan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, newDigest)
	replacementID := driveReplacementHealthy(t, h, newDigest)
	updateRun := waitRunState(t, h, runID, string(runs.Succeeded))
	if updateRun.Progress["completed_devices"] != float64(1) || updateRun.Progress["completed_running_targets"] != float64(1) {
		t.Fatalf("progress = %#v", updateRun.Progress)
	}
	hardware := updateRun.Progress["running_deployments"].([]any)[0].(map[string]any)
	if hardware["current_step"] != float64(5) || hardware["phase"] != "ready" || hardware["replacement_deployment_id"] != replacementID {
		t.Fatalf("running deployment progress = %#v", hardware)
	}
	oldRow := deploymentRow(t, h, source.ID)
	newRow := deploymentRow(t, h, replacementID)
	if oldRow.DesiredState != "stopped" || oldRow.ObservedState != "stopped" {
		t.Fatalf("old deployment = %+v", oldRow)
	}
	if newRow.DesiredState != "running" || newRow.ObservedState != "healthy" {
		t.Fatalf("replacement = %+v", newRow)
	}
	oldPlacement := ParsePlacementSet(oldRow.Placement)
	newPlacement := ParsePlacementSet(newRow.Placement)
	if len(oldPlacement.Entries) != 1 || len(newPlacement.Entries) != 1 || oldPlacement.Entries[0].NodeID != newPlacement.Entries[0].NodeID || oldPlacement.Entries[0].Rank != newPlacement.Entries[0].Rank {
		t.Fatalf("placement changed: old=%+v new=%+v", oldPlacement, newPlacement)
	}
	repository, err := h.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if !repository.CurrentDigest.Valid || repository.CurrentDigest.String != newDigest {
		t.Fatalf("repository current = %+v", repository.CurrentDigest)
	}
}

func TestRepositoryReplacementRequiresExplicitActiveSelection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("8", 64)
	h.seedRecipeUnplaced(t, digest, noArtifactManifest)
	repositoryID := "https://fixtures.local/idle\n."
	seedRepositoryVersion(t, h, repositoryID, digest, strings.Repeat("d", 40), true)
	for _, request := range []RepositoryReplacementRequest{
		{TargetDigest: digest},
		{TargetDigest: digest, DeploymentIDs: []string{""}},
		{TargetDigest: digest, DeploymentIDs: []string{"unknown"}},
		{TargetDigest: digest, DeploymentIDs: []string{"unknown", "unknown"}},
	} {
		if _, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request); !errors.Is(err, ErrRecipe) {
			t.Fatalf("selection %+v error = %v", request, err)
		}
	}
	if commands := h.nodes.artifactCommands(); len(commands) != 0 {
		t.Fatal("invalid selection fetched packages")
	}
}

func TestRepositoryUpdateOutlivesRequestContext(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	oldDigest := "sha256:" + strings.Repeat("4", 64)
	targetDigest := "sha256:" + strings.Repeat("5", 64)
	h.seedRecipeUnplaced(t, targetDigest, noArtifactManifest)
	h.seedNode(t, "node-a", nil, "")
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/request-context\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("8", 40), true)
	seedRepositoryVersion(t, h, repositoryID, targetDigest, strings.Repeat("9", 40), false)
	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	request := RepositoryReplacementRequest{TargetDigest: targetDigest, DeploymentIDs: []string{source.ID}}

	plan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	runID, err := h.svc.CreateRepositoryUpdate(requestCtx, repositoryID, request, plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()

	ackDeploymentStop(t, h, source.ID, targetDigest)
	driveReplacementHealthy(t, h, targetDigest)
	waitRunState(t, h, runID, string(runs.Succeeded))
	repository, err := h.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if !repository.CurrentDigest.Valid || repository.CurrentDigest.String != oldDigest {
		t.Fatalf("repository current = %+v", repository.CurrentDigest)
	}
}

func TestRepositoryUpdateCoordinatorResumesDeviceInstallation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	targetDigest := "sha256:" + strings.Repeat("b", 64)
	h.seedRecipeUnplaced(t, targetDigest, noArtifactManifest)
	h.seedNode(t, "node-a", nil, "")
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/resume\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("1", 40), true)
	seedRepositoryVersion(t, h, repositoryID, targetDigest, strings.Repeat("2", 40), false)
	// Frozen legacy ledger entries still resume their original selected package
	// installations without re-planning moving upstream content.
	plan := &RepositoryUpdatePlan{
		RepositoryID: repositoryID, TargetDigest: targetDigest, Ready: true,
		InstalledDevices: []RepositoryUpdateDevice{{NodeID: "node-a", NodeName: "node-a", NodeStatus: "online", InstalledDigests: []string{oldDigest}}},
	}
	input := repositoryUpdateRunInput{
		RepositoryID: repositoryID, TargetDigest: targetDigest,
		InstalledDevices: append([]RepositoryUpdateDevice(nil), plan.InstalledDevices...),
		Targets:          append([]RepositoryUpdateTarget(nil), plan.RunningDeployments...),
		Plan:             *plan,
	}
	runID, err := h.svc.runs.Create(ctx, "library", "recipe-update", structMap(input), "")
	if err != nil {
		t.Fatal(err)
	}
	progress := repositoryUpdateProgress{
		Phase: "installing_recipe", TotalDevices: len(plan.InstalledDevices),
		InstalledDevices: repositoryUpdateDeviceProgressFrom(plan.InstalledDevices),
	}
	if err := h.svc.runs.SetProgress(ctx, runID, structMap(progress)); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.runs.SetState(ctx, runID, runs.Waiting, "", ""); err != nil {
		t.Fatal(err)
	}

	h.nodes.setOnline("node-a", false)
	h.svc = New(h.dbh, h.q, h.svc.bus, h.svc.runs, h.nodes)
	h.svc.downloads = &fakeAcquisition{service: h.svc}
	h.svc.RunRepositoryUpdateCoordinator(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		updateRun, getErr := h.svc.runs.Get(ctx, runID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		devices, _ := updateRun.Progress["installed_devices"].([]any)
		if len(devices) == 1 {
			device, _ := devices[0].(map[string]any)
			if device["phase"] == "waiting_offline" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("update did not wait for reconnect: state=%s progress=%#v", updateRun.State, updateRun.Progress)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if commands := h.nodes.artifactCommands(); len(commands) != 0 {
		t.Fatalf("sent package fetch while node was offline: %+v", commands)
	}
	h.nodes.setOnline("node-a", true)
	ackRecipeUpdateFetch(t, h, targetDigest, "node-a")
	waitRunState(t, h, runID, string(runs.Succeeded))
	repository, err := h.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if !repository.CurrentDigest.Valid || repository.CurrentDigest.String != targetDigest {
		t.Fatalf("resumed update current = %+v", repository.CurrentDigest)
	}
}

func TestRepositoryReplacementIgnoresUnselectedOfflineDevices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	oldDigest := "sha256:" + strings.Repeat("c", 64)
	targetDigest := "sha256:" + strings.Repeat("d", 64)
	h.seedNode(t, "node-a", nil, "")
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	h.seedRecipeUnplaced(t, targetDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/offline\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("3", 40), true)
	seedRepositoryVersion(t, h, repositoryID, targetDigest, strings.Repeat("4", 40), false)
	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	h.seedNode(t, "node-b", nil, "")
	if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{
		ID: "node-b", Inventory: nullString(strings.ReplaceAll(inventoryWith(nil, ""), "100.86.3.45", "100.86.3.46")),
	}); err != nil {
		t.Fatal(err)
	}
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	otherPlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest, Placements: []PlacementOverride{{NodeID: "node-b", Rank: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, Placements: []PlacementOverride{{NodeID: "node-b", Rank: 0}}, PlanDigest: otherPlan.Digest})
	if err != nil {
		t.Fatalf("create unrelated deployment: %v; reviewed plan: %+v", err, otherPlan)
	}
	driveDeploymentHealthy(t, h, other.ID, "node-b")
	if err := h.q.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "offline", ID: "node-b"}); err != nil {
		t.Fatal(err)
	}
	request := RepositoryReplacementRequest{TargetDigest: targetDigest, DeploymentIDs: []string{source.ID}}
	plan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || len(plan.Deployments) != 1 || plan.Deployments[0].SourceDeploymentID != source.ID ||
		len(plan.InstalledDevices) != 1 || plan.InstalledDevices[0].NodeID != "node-a" {
		t.Fatalf("unselected device affected plan: %+v", plan)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, targetDigest)
	driveReplacementHealthy(t, h, targetDigest)
	waitRunState(t, h, runID, string(runs.Succeeded))
	if row := deploymentRow(t, h, other.ID); row.DesiredState != "running" || row.RecipeDigest != oldDigest {
		t.Fatalf("unselected deployment changed: %+v", row)
	}
}

func TestRepositoryReplacementCancellationWaitsForAcquisitionQuiescence(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("e", 64)
	targetDigest := "sha256:" + strings.Repeat("f", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	h.seedRecipeUnplaced(t, targetDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/acquisition-cancel\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("5", 40), true)
	seedRepositoryVersion(t, h, repositoryID, targetDigest, strings.Repeat("6", 40), true)
	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	h.svc.downloads = &fakeAcquisition{acquire: func(acquireCtx context.Context, _ downloads.PlanRequest, _ bool) error {
		close(started)
		<-acquireCtx.Done()
		close(cancelled)
		<-release
		return acquireCtx.Err()
	}}
	request := RepositoryReplacementRequest{TargetDigest: targetDigest, DeploymentIDs: []string{source.ID}}
	plan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition not started")
	}
	if err := h.svc.CancelRepositoryUpdate(ctx, runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition did not receive cancellation")
	}
	run, err := h.svc.runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != string(runs.Cancelling) {
		t.Fatalf("completed before acquisition stopped: %s", run.State)
	}
	if row := deploymentRow(t, h, source.ID); row.DesiredState != "running" || row.ObservedState != "healthy" {
		t.Fatalf("acquisition stopped source: %+v", row)
	}
	release <- struct{}{}
	waitRunState(t, h, runID, string(runs.Cancelled))
	repository, err := h.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if repository.CurrentDigest.String != targetDigest {
		t.Fatal("acquisition cancellation reverted catalog save")
	}
}

func TestRepositoryReplacementAlreadyTargetDoesNotRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	digest := "sha256:" + strings.Repeat("0", 64)
	h.seedRecipe(t, digest, noArtifactManifest)
	repositoryID := "https://fixtures.local/unchanged\n."
	seedRepositoryVersion(t, h, repositoryID, digest, strings.Repeat("7", 40), true)
	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: digest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	request := RepositoryReplacementRequest{TargetDigest: digest, DeploymentIDs: []string{source.ID}}
	plan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || len(plan.Deployments) != 0 || len(plan.UnchangedDeploymentIDs) != 1 || plan.UnchangedDeploymentIDs[0] != source.ID {
		t.Fatalf("already target was not reported unchanged: %+v", plan)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	waitRunState(t, h, runID, string(runs.Succeeded))
	rows, err := h.q.ListDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != source.ID || rows[0].ObservedState != "healthy" {
		t.Fatalf("no-change selection restarted deployment: %+v", rows)
	}
}

func TestReplacementAcquisitionAllowsOnlyVerifiedReuseTransition(t *testing.T) {
	now := time.Now().UTC()
	approvedResource := downloads.Resource{
		ResourceSpec: downloads.ResourceSpec{Kind: downloads.ResourceArtifact, Identity: "hf://fixture/model@" + strings.Repeat("a", 40), Destination: "/cache/exact"},
		Key:          "exact-model", NodeID: "node-a", Required: true,
		Action: downloads.ActionPeerCopy, SourceNode: "node-b", SourcePath: "/cache/peer", CredentialID: "approved-secret",
	}
	reviewed := &Plan{RecipeDigest: "recipe", AcquisitionPolicy: AcquisitionDownloadMissing, Acquisition: &downloads.Plan{
		Targets:   []downloads.Target{{NodeID: "node-a", CacheRoot: "/cache"}},
		Resources: []downloads.Resource{approvedResource},
	}}
	freshResource := approvedResource
	freshResource.Action, freshResource.SourceNode, freshResource.SourcePath, freshResource.CredentialID = downloads.ActionReuse, "", "", ""
	freshResource.Verification = downloads.Verification{State: downloads.ResourceAvailable, VerifiedAt: &now}
	fresh := *reviewed
	freshAcquisition := *reviewed.Acquisition
	freshAcquisition.Resources = []downloads.Resource{freshResource}
	fresh.Acquisition = &freshAcquisition
	if !replacementPlanMatchesAfterAcquisition(reviewed, &fresh) {
		t.Fatal("verified acquired bytes could not replace approved peer acquisition")
	}
	if fresh.Acquisition.Resources[0].Action != downloads.ActionReuse {
		t.Fatal("comparison rewrote execution's real acquisition state")
	}
	fresh.Acquisition.Resources[0].Verification.Stale = true
	if replacementPlanMatchesAfterAcquisition(reviewed, &fresh) {
		t.Fatal("stale observation authorized replacement")
	}
	fresh.Acquisition.Resources[0] = freshResource
	fresh.Acquisition.Resources[0].Action = downloads.ActionDownloadOrigin
	if replacementPlanMatchesAfterAcquisition(reviewed, &fresh) {
		t.Fatal("peer disappearance silently switched to origin")
	}
	fresh.Acquisition.Resources[0] = freshResource
	fresh.Acquisition.Resources[0].Destination = "/other-cache"
	if replacementPlanMatchesAfterAcquisition(reviewed, &fresh) {
		t.Fatal("reuse silently changed destination")
	}
	fresh.Acquisition.Resources[0] = freshResource
	fresh.Acquisition.Resources[0].Identity = "hf://fixture/model@" + strings.Repeat("b", 40)
	if replacementPlanMatchesAfterAcquisition(reviewed, &fresh) {
		t.Fatal("reuse substituted another immutable revision")
	}
}

func ackRecipeUpdateFetch(t *testing.T, h *harness, digest, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, sent := range h.nodes.artifactCommands() {
			command := sent.msg.GetArtifactCommand()
			if sent.nodeID != nodeID || command.GetArtifactIdentity() != "recipe://"+digest {
				continue
			}
			artifact, err := h.q.GetArtifactByIdentity(context.Background(), command.GetArtifactIdentity())
			if err != nil {
				t.Fatal(err)
			}
			if err := h.q.UpsertPlacement(context.Background(), db.UpsertPlacementParams{
				ArtifactID: artifact.ID, NodeID: nodeID, Path: "/var/lib/lmw/recipes/" + strings.TrimPrefix(digest, "sha256:"),
				State: "valid", Diagnostics: "[]",
			}); err != nil {
				t.Fatal(err)
			}
			h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for repository package fetch")
}

func seedRepositoryVersion(t *testing.T, h *harness, repositoryID, digest, commit string, current bool) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := h.q.UpsertRecipeRepository(context.Background(), db.UpsertRecipeRepositoryParams{
		ID: repositoryID, SourceUrl: strings.Split(repositoryID, "\n")[0], SourcePath: ".",
		TrackingRef: "HEAD", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.q.AttachRecipeRepositoryVersion(context.Background(), db.AttachRecipeRepositoryVersionParams{
		RepositoryID: repositoryID, RecipeDigest: digest, CommitSha: commit,
		TreeSha: sql.NullString{}, Canonical: 1, InstalledAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if current {
		if err := h.q.SetRecipeRepositoryCurrent(context.Background(), db.SetRecipeRepositoryCurrentParams{
			CurrentDigest: sql.NullString{String: digest, Valid: true}, UpdatedAt: now, ID: repositoryID,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func driveDeploymentHealthy(t *testing.T, h *harness, deploymentID, nodeID string) {
	t.Helper()
	h.svc.readinessProbe = func(context.Context, db.GetDeploymentRow) (bool, string) {
		return true, ""
	}
	deadline := time.Now().Add(5 * time.Second)
	acked := map[string]bool{}
	for time.Now().Before(deadline) {
		for _, sent := range h.nodes.workloadCommands() {
			command := sent.msg.GetWorkloadCommand()
			if command.GetDeploymentId() != deploymentID || acked[command.GetCommandId()] {
				continue
			}
			acked[command.GetCommandId()] = true
			h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
		}
		row := deploymentRow(t, h, deploymentID)
		if ParseDispatch(row.Dispatch).Get(0) == PhaseStarted {
			h.svc.OnStateUpdate(context.Background(), nodeID, &agentv1.StateUpdate{
				DeploymentId: deploymentID, ContainerId: deploymentID, State: "running", Rank: 0,
			})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment %s did not reach started", deploymentID)
}

func ackDeploymentStop(t *testing.T, h *harness, deploymentID, targetDigest string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, sent := range h.nodes.workloadCommands() {
			command := sent.msg.GetWorkloadCommand()
			if command.GetDeploymentId() != deploymentID || command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
				continue
			}
			deployments, err := h.q.ListDeployments(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, deployment := range deployments {
				if deployment.RecipeDigest == targetDigest {
					t.Fatalf("replacement %s created before source stop acknowledgement", deployment.ID)
				}
			}
			h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment %s did not receive stop", deploymentID)
}

func driveReplacementHealthy(t *testing.T, h *harness, targetDigest string) string {
	t.Helper()
	h.svc.readinessProbe = func(context.Context, db.GetDeploymentRow) (bool, string) {
		return true, ""
	}
	deadline := time.Now().Add(5 * time.Second)
	acked := map[string]bool{}
	replacementID := ""
	for time.Now().Before(deadline) {
		deployments, err := h.q.ListDeployments(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, deployment := range deployments {
			if deployment.RecipeDigest == targetDigest {
				replacementID = deployment.ID
			}
		}
		if replacementID != "" {
			for _, sent := range h.nodes.workloadCommands() {
				command := sent.msg.GetWorkloadCommand()
				if command.GetDeploymentId() != replacementID || acked[command.GetCommandId()] {
					continue
				}
				acked[command.GetCommandId()] = true
				h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
			}
			row := deploymentRow(t, h, replacementID)
			if ParseDispatch(row.Dispatch).Get(0) == PhaseStarted {
				h.svc.OnStateUpdate(context.Background(), "node-a", &agentv1.StateUpdate{
					DeploymentId: replacementID, ContainerId: "replacement", State: "running", Rank: 0,
				})
				return replacementID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	deployments, _ := h.q.ListDeployments(context.Background())
	t.Fatalf("replacement did not reach started: replacement=%s deployments=%+v commands=%d", replacementID, deployments, len(h.nodes.workloadCommands()))
	return ""
}

func waitRunState(t *testing.T, h *harness, runID, state string) runs.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.svc.runs.Get(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.State == state {
			return run
		}
		if runs.State(run.State).Terminal() {
			t.Fatalf("run terminated as %s: %v %v", run.State, run.ErrorCode, run.ErrorMessage)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", runID, state)
	return runs.Run{}
}
