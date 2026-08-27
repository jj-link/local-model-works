package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/runs"
)

func TestRepositoryUpdateCancellationRestoresSource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("3", 64)
	newDigest := "sha256:" + strings.Repeat("4", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	h.seedRecipe(t, newDigest, noArtifactManifest)
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
	updatePlan, err := h.svc.PlanRepositoryUpdate(ctx, repositoryID, newDigest)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := h.svc.CreateRepositoryUpdate(ctx, repositoryID, newDigest, updatePlan.Digest)
	if err != nil {
		t.Fatal(err)
	}
	ackDeploymentStop(t, h, source.ID, newDigest)
	replacementID := waitDeploymentDigest(t, h, newDigest)
	if err := h.svc.CancelRepositoryUpdate(ctx, runID); err != nil {
		t.Fatal(err)
	}
	driveDeploymentHealthy(t, h, source.ID, "node-a")
	cancelled := waitRunState(t, h, runID, string(runs.Cancelled))
	if cancelled.Progress["phase"] != "restored" {
		t.Fatalf("cancel progress = %#v", cancelled.Progress)
	}
	sourceRow := deploymentRow(t, h, source.ID)
	replacementRow := deploymentRow(t, h, replacementID)
	if sourceRow.DesiredState != "running" || sourceRow.ObservedState != "healthy" {
		t.Fatalf("source not restored: %+v", sourceRow)
	}
	if replacementRow.DesiredState != "stopped" || replacementRow.ObservedState != "stopped" {
		t.Fatalf("replacement not rolled back: %+v", replacementRow)
	}
}

func waitDeploymentDigest(t *testing.T, h *harness, digest string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := h.q.ListDeployments(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.RecipeDigest == digest {
				return row.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("deployment for %s was not created", digest)
	return ""
}
