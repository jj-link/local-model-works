package deploy

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
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
	h.seedRecipe(t, newDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/recipe\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("a", 40), true)
	seedRepositoryVersion(t, h, repositoryID, newDigest, strings.Repeat("b", 40), true)

	sourcePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: oldDigest})
	if err != nil {
		t.Fatal(err)
	}
	source, err := h.svc.Create(ctx, CreateRequest{RecipeDigest: oldDigest, PlanDigest: sourcePlan.Digest})
	if err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	updatePlan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !updatePlan.Ready || len(updatePlan.Targets) != 1 {
		t.Fatalf("update plan = %+v", updatePlan)
	}
	if target := updatePlan.Targets[0]; target.NodeID != "node-a" || target.SourceDeploymentID != source.ID || target.Rank != 0 {
		t.Fatalf("target = %+v", target)
	}
	if _, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, newDigest, "sha256:stale"); !errors.Is(err, ErrPlanStale) {
		t.Fatalf("stale plan error = %v", err)
	}
	if row := deploymentRow(t, h, source.ID); row.DesiredState != "running" {
		t.Fatalf("stale plan stopped source: %+v", row)
	}

	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, newDigest, updatePlan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, newDigest)
	replacementID := driveReplacementHealthy(t, h, newDigest)
	updateRun := waitRunState(t, h, runID, string(runs.Succeeded))
	if updateRun.Progress["completed_hardware"] != float64(1) {
		t.Fatalf("progress = %#v", updateRun.Progress)
	}
	hardware := updateRun.Progress["hardware"].([]any)[0].(map[string]any)
	if hardware["current_step"] != float64(5) || hardware["phase"] != "ready" || hardware["replacement_deployment_id"] != replacementID {
		t.Fatalf("hardware progress = %#v", hardware)
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
