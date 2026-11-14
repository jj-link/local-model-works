package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// Cancelling after the deployment was restarted on the new recipe stops it and
// leaves it stopped: nothing is restored and no other deployment appears.
func TestRepositoryUpdateCancellationLeavesDeploymentStopped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("3", 64)
	newDigest := "sha256:" + strings.Repeat("4", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	h.seedRecipeUnplaced(t, newDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/cancel\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("c", 40), true)
	seedRepositoryVersion(t, h, repositoryID, newDigest, strings.Repeat("d", 40), true)

	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	request := RepositoryReplacementRequest{TargetDigest: newDigest, DeploymentIDs: []string{source.ID}}
	updatePlan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, updatePlan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, newDigest)
	if updatedID := waitDeploymentRunning(t, h, newDigest); updatedID != source.ID {
		t.Fatalf("update started %s instead of the deployment itself", updatedID)
	}
	restartRun := deploymentRow(t, h, source.ID).RunID.String
	if err := h.svc.CancelRepositoryUpdate(ctx, runID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, sent := range h.nodes.workloadCommands() {
			command := sent.msg.GetWorkloadCommand()
			if command.GetDeploymentId() == source.ID && command.GetRunId() == restartRun && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_STOP {
				h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
			}
		}
		row := deploymentRow(t, h, source.ID)
		if row.DesiredState == "stopped" && row.ObservedState == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled update did not stop the deployment: %+v", row)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelled := waitRunState(t, h, runID, string(runs.Cancelled))
	if cancelled.Progress["phase"] != "failed" {
		t.Fatalf("cancel progress = %#v", cancelled.Progress)
	}
	// Give a restore, if one were wrongly attempted, time to show.
	time.Sleep(4 * repositoryUpdatePollInterval)
	rows, err := h.q.ListDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("cancelled update left %d deployments, want 1", len(rows))
	}
	if row := deploymentRow(t, h, source.ID); row.RecipeDigest != newDigest || row.DesiredState != "stopped" || row.ObservedState != "stopped" {
		t.Fatalf("deployment was started again after cancel: %+v", row)
	}
}

// waitDeploymentRunning returns the deployment on digest once it is started.
func waitDeploymentRunning(t *testing.T, h *harness, digest string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := h.q.ListDeployments(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.RecipeDigest == digest && row.DesiredState == "running" {
				return row.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment on %s was not started", digest)
	return ""
}

// A deployment that does not start on the new recipe is stopped and stays
// stopped; the previous version is not started in its place.
func TestRepositoryUpdateFailedStartLeavesDeploymentStopped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("5", 64)
	newDigest := "sha256:" + strings.Repeat("6", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	h.seedRecipeUnplaced(t, newDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/failed-start\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("e", 40), true)
	seedRepositoryVersion(t, h, repositoryID, newDigest, strings.Repeat("f", 40), true)
	source := h.createDeployment(t, oldDigest)
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	firstRun := deploymentRow(t, h, source.ID).RunID.String
	request := RepositoryReplacementRequest{TargetDigest: newDigest, DeploymentIDs: []string{source.ID}}
	plan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, request)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, request, plan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, newDigest)
	waitDeploymentRunning(t, h, newDigest)
	failLaunch(t, h, source.ID, firstRun)
	failed := waitRunState(t, h, runID, string(runs.Failed))
	if failed.ErrorCode == nil || *failed.ErrorCode != "recipe.update_failed" {
		t.Fatalf("failed update error = %v", failed.ErrorCode)
	}
	time.Sleep(4 * repositoryUpdatePollInterval)
	rows, err := h.q.ListDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("failed update left %d deployments, want 1", len(rows))
	}
	if row := deploymentRow(t, h, source.ID); row.RecipeDigest != newDigest || row.DesiredState != "stopped" || row.ObservedState == "healthy" {
		t.Fatalf("deployment after failed update = %+v", row)
	}
}
