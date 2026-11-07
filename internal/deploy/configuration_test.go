package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestConfigurationChangesTheSameDeployment(t *testing.T) {
	for _, initialState := range []string{"running", "stopped"} {
		for _, outcome := range []string{"healthy", "failed"} {
			if initialState == "stopped" && outcome == "failed" {
				continue // a stopped deployment is not started, so cannot fail to
			}
			t.Run(initialState+"/"+outcome, func(t *testing.T) {
				h := newHarness(t)
				ctx := context.Background()
				h.seedNode(t, "node-a", nil, "")
				digest := "sha256:" + strings.Repeat("7", 64)
				manifest, err := recipe.Parse([]byte(noArtifactManifest))
				if err != nil {
					t.Fatal(err)
				}
				manifest.Parameters = []recipe.Parameter{{Name: "context_length", Type: "int", Default: 4096}}
				manifest.Workloads[0].Args = append(manifest.Workloads[0].Args, "--context-length", "${setting.context_length}")
				document, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				h.seedRecipe(t, digest, string(document))
				source := h.createDeployment(t, digest)
				driveDeploymentHealthy(t, h, source.ID, "node-a")
				if initialState == "stopped" {
					if _, err := h.svc.Stop(ctx, source.ID); err != nil {
						t.Fatal(err)
					}
					ackDeploymentStop(t, h, source.ID, digest)
				}
				original := deploymentRow(t, h, source.ID)
				settings := RepositoryReplacementSettings{Parameters: map[string]any{"context_length": 8192}}
				plan, err := h.svc.PlanDeploymentConfiguration(ctx, source.ID, settings)
				if err != nil || !plan.Ready || len(plan.Deployments) != 1 {
					t.Fatalf("configuration not reviewable: plan=%+v err=%v", plan, err)
				}
				if _, err := h.svc.CreateDeploymentConfiguration(ctx, source.ID, RepositoryReplacementSettings{Parameters: map[string]any{"context_length": 16384}}, plan.Digest); !errors.Is(err, ErrPlanStale) {
					t.Fatalf("different settings accepted against reviewed plan: %v", err)
				}
				if row := deploymentRow(t, h, source.ID); row.DesiredState != original.DesiredState || row.Parameters != original.Parameters {
					t.Fatal("stale submission changed the deployment")
				}
				runID, err := h.svc.CreateDeploymentConfiguration(ctx, source.ID, settings, plan.Digest)
				if err != nil {
					t.Fatal(err)
				}
				if initialState == "running" {
					ackDeploymentStop(t, h, source.ID, digest)
					if outcome == "healthy" {
						driveDeploymentHealthy(t, h, source.ID, "node-a")
					} else {
						failLaunch(t, h, source.ID, original.RunID.String)
					}
				}
				waitRunState(t, h, runID, map[string]string{"healthy": string(runs.Succeeded), "failed": string(runs.Failed)}[outcome])

				rows, err := h.q.ListDeployments(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 {
					t.Fatalf("configuration created another deployment: %d deployments", len(rows))
				}
				row := deploymentRow(t, h, source.ID)
				if parametersForValue(row.Parameters)["context_length"] != float64(8192) {
					t.Fatalf("deployment did not keep the new setting: %s", row.Parameters)
				}
				if preservedPlacementMismatch(ParsePlacementSet(original.Placement).Entries, ParsePlacementSet(row.Placement).Entries) != "" || row.Fabric != original.Fabric {
					t.Fatal("configuration changed deployment hardware or fabric")
				}
				want := map[string]string{"running/healthy": "healthy", "stopped/healthy": "stopped"}[initialState+"/"+outcome]
				if outcome == "failed" {
					if row.ObservedState == "healthy" {
						t.Fatal("a failed start brought a deployment back")
					}
				} else if row.ObservedState != want {
					t.Fatalf("deployment is %s, want %s", row.ObservedState, want)
				}
				if _, err := h.q.GetRecipeRepositoryVersionByDigest(ctx, digest); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("configuration manufactured repository ownership: %v", err)
				}
			})
		}
	}
}

// failLaunch fails the first launch command of the deployment's new run.
func failLaunch(t *testing.T, h *harness, deploymentID, previousRunID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, sent := range h.nodes.workloadCommands() {
			command := sent.msg.GetWorkloadCommand()
			if command.GetDeploymentId() == deploymentID && command.GetRunId() != previousRunID && command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_STOP {
				h.svc.OnCommandResult(context.Background(), &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: false, Error: "launch failed"})
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("restart did not dispatch a launch command")
}

func TestConfigurationReviewBindsLiveSourceContract(t *testing.T) {
	plan := &RepositoryUpdatePlan{Deployments: []RepositoryUpdateDeployment{{SourceDeploymentID: "source", SourceDigest: "recipe", Parameters: map[string]any{"context_length": 4096}, Placement: "original", Fabric: "fabric"}}}
	for _, mutate := range []func(*RepositoryUpdateDeployment){
		func(source *RepositoryUpdateDeployment) { source.Parameters = map[string]any{"context_length": 2048} },
		func(source *RepositoryUpdateDeployment) { source.Placement = "changed" },
		func(source *RepositoryUpdateDeployment) { source.Fabric = "changed" },
		func(source *RepositoryUpdateDeployment) { source.SourceWasStopped = true },
	} {
		before := plan.PlanDigest()
		mutate(&plan.Deployments[0])
		if plan.PlanDigest() == before {
			t.Fatal("source changed without invalidating reviewed configuration")
		}
	}
}
