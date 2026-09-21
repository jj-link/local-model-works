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

func TestStandaloneConfigurationApplyAndRollback(t *testing.T) {
	for _, initialState := range []string{"running", "stopped"} {
		for _, outcome := range []string{"healthy", "cancelled", "failed"} {
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
					t.Fatalf("standalone configuration not reviewable: plan=%+v err=%v", plan, err)
				}
				if plan.RepositoryID != "" || plan.TargetDigest != digest || plan.Deployments[0].SourceWasStopped != (initialState == "stopped") {
					t.Fatalf("configuration changed ownership or source state: %+v", plan)
				}
				if _, err := h.svc.CreateDeploymentConfiguration(ctx, source.ID, RepositoryReplacementSettings{Parameters: map[string]any{"context_length": 16384}}, plan.Digest); !errors.Is(err, ErrPlanStale) {
					t.Fatalf("different settings accepted against reviewed plan: %v", err)
				}
				if row := deploymentRow(t, h, source.ID); row.DesiredState != original.DesiredState || row.Parameters != original.Parameters {
					t.Fatal("stale submission changed source deployment")
				}
				runID, err := h.svc.CreateDeploymentConfiguration(ctx, source.ID, settings, plan.Digest)
				if err != nil {
					t.Fatal(err)
				}
				if initialState == "running" {
					ackDeploymentStop(t, h, source.ID, digest)
				}
				if outcome == "healthy" {
					replacementID := driveReplacementHealthy(t, h, digest)
					waitRunState(t, h, runID, string(runs.Succeeded))
					replacement := deploymentRow(t, h, replacementID)
					if replacementID == source.ID || replacement.RecipeDigest != digest || parametersForValue(replacement.Parameters)["context_length"] != float64(8192) {
						t.Fatalf("replacement lost the reviewed recipe or setting: %+v", replacement)
					}
					if preservedPlacementMismatch(ParsePlacementSet(original.Placement).Entries, ParsePlacementSet(replacement.Placement).Entries) != "" || replacement.Fabric != original.Fabric {
						t.Fatal("configuration changed deployment hardware or fabric")
					}
					if row := deploymentRow(t, h, source.ID); row.Parameters != original.Parameters || row.DesiredState != "stopped" {
						t.Fatal("configuration discarded the retained source")
					}
				} else {
					// Cancel only after a replacement exists, so rollback must stop
					// it before restoring the original running/stopped source state.
					replacementID := ""
					deadline := time.Now().Add(5 * time.Second)
					for replacementID == "" && time.Now().Before(deadline) {
						rows, err := h.q.ListDeployments(ctx)
						if err != nil {
							t.Fatal(err)
						}
						for _, row := range rows {
							if row.ID != source.ID && row.RecipeDigest == digest {
								replacementID = row.ID
							}
						}
						if replacementID == "" {
							time.Sleep(10 * time.Millisecond)
						}
					}
					if replacementID == "" {
						t.Fatal("replacement was not created")
					}
					if outcome == "cancelled" {
						if err := h.svc.CancelRepositoryUpdate(ctx, runID); err != nil {
							t.Fatal(err)
						}
					} else {
						failed := false
						deadline = time.Now().Add(5 * time.Second)
						for !failed && time.Now().Before(deadline) {
							for _, sent := range h.nodes.workloadCommands() {
								command := sent.msg.GetWorkloadCommand()
								if command.GetDeploymentId() == replacementID {
									h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: false, Error: "replacement launch failed"})
									failed = true
									break
								}
							}
							if !failed {
								time.Sleep(10 * time.Millisecond)
							}
						}
						if !failed {
							t.Fatal("replacement did not dispatch a launch command")
						}
					}
					deadline = time.Now().Add(5 * time.Second)
					for {
						for _, sent := range h.nodes.workloadCommands() {
							command := sent.msg.GetWorkloadCommand()
							if command.GetDeploymentId() == replacementID && command.GetOp() == agentv1.WorkloadOp_WORKLOAD_OP_STOP {
								h.svc.OnCommandResult(ctx, &agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true})
							}
						}
						row := deploymentRow(t, h, replacementID)
						if row.DesiredState == "stopped" && row.ObservedState == "stopped" {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("replacement did not stop: %+v", row)
						}
						time.Sleep(10 * time.Millisecond)
					}
					if initialState == "running" {
						driveDeploymentHealthy(t, h, source.ID, "node-a")
					}
					waitRunState(t, h, runID, outcome)
					restored := deploymentRow(t, h, source.ID)
					if restored.DesiredState != original.DesiredState || restored.ObservedState != original.ObservedState || restored.Parameters != original.Parameters || restored.Fabric != original.Fabric || preservedPlacementMismatch(ParsePlacementSet(original.Placement).Entries, ParsePlacementSet(restored.Placement).Entries) != "" {
						t.Fatalf("rollback lost original source contract: original=%+v restored=%+v", original, restored)
					}
				}
				if _, err := h.q.GetRecipeRepositoryVersionByDigest(ctx, digest); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("standalone configuration manufactured repository ownership: %v", err)
				}
			})
		}
	}
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
