package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/workerssh"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func enableWorkerSSH(t *testing.T, h *harness, nodeID string) {
	t.Helper()
	inv, err := h.svc.workerSSHInventory(context.Background(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	inv.ProtocolFeatures = append(inv.ProtocolFeatures, workerssh.ProtocolFeature)
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: nodeID, Inventory: nullString(string(raw))}); err != nil {
		t.Fatal(err)
	}
}

func installWorkerSSHResponder(t *testing.T, h *harness, respond func(*agentv1.WorkerSSHCommand) (workerssh.Result, error)) {
	t.Helper()
	broker := commands.New()
	h.svc.SetCommands(broker)
	h.nodes.onSend = func(message *agentv1.ServerMessage) {
		command := message.GetWorkerSshCommand()
		if command == nil {
			return
		}
		result, err := respond(command)
		ack := &agentv1.CommandResult{CommandId: command.CommandId, Ok: err == nil}
		if err != nil {
			ack.Error = err.Error()
		} else {
			ack.OutputJson, err = json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
		}
		broker.Deliver(ack)
	}
}

func successfulWorkerSSH(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
	target, username := command.Target, command.Username
	if target == "" {
		target = username + "@" + command.Addresses[0]
	} else if user, _, ok := strings.Cut(target, "@"); ok {
		username = user
	}
	return workerssh.Result{Target: target, Hostname: command.Addresses[0], Username: username}, nil
}

func newWorkerSSHHarness(t *testing.T) *harness {
	t.Helper()
	h := newUpstreamHarness(t)
	enableWorkerSSH(t, h, "head")
	installWorkerSSHResponder(t, h, successfulWorkerSSH)
	return h
}

func TestWorkerSSHAliasDefaultsRemainInferredAndStable(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", deviceSettingsManifest(t))
	resolutions, probes := 0, 0
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		if command.ResolveOnly {
			resolutions++
		} else {
			probes++
			if command.Target != "configured-worker" {
				t.Fatalf("source destination was rewritten: %q", command.Target)
			}
		}
		return workerssh.Result{Target: "configured-worker", Hostname: "192.0.2.2", Username: "alice"}, nil
	})
	ctx := context.Background()
	request := PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()}
	plan, err := h.svc.Plan(ctx, request)
	if err != nil || !plan.Ready {
		t.Fatalf("configured SSH alias did not yield readiness: %+v, %v", plan, err)
	}
	if plan.Parameters["login"] != "configured-worker" || plan.Parameters["target"] != "configured-worker" || plan.Upstream[0].Environment["WORKER_SSH"] != "configured-worker" {
		t.Fatalf("resolved alias is absent from the reviewed launch: %+v", plan)
	}
	if resolutions != 1 || probes != 1 {
		t.Fatalf("identical head/worker bindings repeated preflight: resolve=%d probe=%d", resolutions, probes)
	}
	again, err := h.svc.Plan(ctx, request)
	if err != nil || again.Digest != plan.Digest || request.Parameters != nil {
		t.Fatalf("inferred alias is unstable or became an override: %v", err)
	}
	profile, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{Name: "portable", RecipeDigest: request.RecipeDigest})
	if err != nil || len(profile.Parameters) != 0 {
		t.Fatalf("saved profile pinned an inferred alias: %+v, %v", profile, err)
	}
}

func TestWorkerSSHChecksExplicitAndProfileTargetsUnchanged(t *testing.T) {
	for _, profile := range []bool{false, true} {
		t.Run(fmt.Sprintf("profile=%t", profile), func(t *testing.T) {
			h := newWorkerSSHHarness(t)
			setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
			seedDeviceSettingsRecipe(t, h, "ssh-recipe", deviceSettingsManifest(t))
			bad := "operator@192.0.2.2"
			installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
				if !command.ResolveOnly && command.Target == bad {
					return workerssh.Result{}, errors.New("Permission denied (publickey)")
				}
				return successfulWorkerSSH(command)
			})
			request := PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements(), Parameters: map[string]any{"login": bad}}
			if profile {
				saved, err := h.svc.CreateLaunchProfile(context.Background(), UpsertLaunchProfileRequest{Name: "explicit", RecipeDigest: request.RecipeDigest, Parameters: request.Parameters})
				if err != nil {
					t.Fatal(err)
				}
				request.LaunchProfileID, request.Parameters = saved.ID, nil
			}
			plan, err := h.svc.Plan(context.Background(), request)
			if err != nil || plan.Ready || plan.Parameters["login"] != bad || !strings.Contains(fmt.Sprint(plan.Diagnostics), "Permission denied") {
				t.Fatalf("explicit unreachable target was corrected or approved: %+v, %v", plan, err)
			}
		})
	}
}

func TestWorkerSSHFailuresBlockReadiness(t *testing.T) {
	for _, failure := range []string{"old-agent", "offline-head", "offline-worker", "missing-broker", "mismatched-host", "rewritten-target", "invalid-result", "untrusted-host", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			h := newWorkerSSHHarness(t)
			setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
			manifest := deviceSettingsManifest(t)
			seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
			request := PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements(), Parameters: map[string]any{"login": "existing-alias", "target": "existing-alias"}}
			ctx := context.Background()
			switch failure {
			case "old-agent":
				setUpstreamNodeInventory(t, h, "head", "192.0.2.1", true)
			case "offline-head":
				h.nodes.setOnline("head", false)
			case "offline-worker":
				h.nodes.setOnline("worker", false)
			case "missing-broker":
				h.svc.SetCommands(nil)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				h.nodes.onSend = func(*agentv1.ServerMessage) { cancel() }
				defer cancel()
			default:
				installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
					result, _ := successfulWorkerSSH(command)
					switch failure {
					case "mismatched-host":
						result.Hostname = "198.51.100.99"
					case "rewritten-target":
						result.Target = "corrected-alias"
					case "invalid-result":
						return workerssh.Result{}, nil
					case "untrusted-host":
						return workerssh.Result{}, errors.New("Host key verification failed")
					}
					return result, nil
				})
			}
			plan, err := h.svc.Plan(ctx, request)
			if err != nil || plan.Ready || !strings.Contains(fmt.Sprint(plan.Diagnostics), "upstream.worker_ssh_unavailable") {
				t.Fatalf("SSH failure did not block launch: %+v, %v", plan, err)
			}
		})
	}
}

func TestWorkerSSHDefaultNeedsCapableHead(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setUpstreamNodeInventory(t, h, "head", "192.0.2.1", true)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", deviceSettingsManifest(t))
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()})
	if err != nil || plan.Ready || !strings.Contains(fmt.Sprint(plan.Diagnostics), workerssh.ProtocolFeature) {
		t.Fatalf("old head silently guessed an SSH default: %+v, %v", plan, err)
	}
}

func TestWorkerSSHUsesSelectedCoordinatorAndWorker(t *testing.T) {
	h := newWorkerSSHHarness(t)
	h.seedNode(t, "new-head", nil, "")
	setUpstreamNodeInventory(t, h, "new-head", "192.0.2.3", true)
	enableWorkerSSH(t, h, "new-head")
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	manifest := deviceSettingsManifest(t)
	manifest.Compatibility.NodeCount = 3
	manifest.Workloads[0].Ranks = []int{0, 1, 2}
	coordinator := 2
	manifest.Workloads[0].Upstream.CoordinatorRank = &coordinator
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		return workerssh.Result{Target: "stale-alias", Hostname: "192.0.2.2", Username: "alice"}, nil
	})
	request := PlanRequest{RecipeDigest: "ssh-recipe", Placements: append(upstreamPlacements(), PlacementOverride{NodeID: "new-head", Rank: 2})}
	plan, err := h.svc.Plan(context.Background(), request)
	if err != nil || !plan.Ready {
		t.Fatalf("nonzero coordinator failed: %+v, %v", plan, err)
	}
	for _, sent := range h.nodes.msgs {
		if sent.msg.GetWorkerSshCommand() != nil && sent.nodeID != "new-head" {
			t.Fatalf("SSH configuration was read from non-coordinator %q", sent.nodeID)
		}
	}
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "198.51.100.2", nil)
	request.Parameters = map[string]any{"login": "stale-alias", "target": "stale-alias"}
	stale, err := h.svc.Plan(context.Background(), request)
	if err != nil || stale.Ready || !strings.Contains(fmt.Sprint(stale.Diagnostics), "not selected worker") {
		t.Fatalf("stale head alias escaped selected-worker validation: %+v, %v", stale, err)
	}
}

func TestWorkerSSHChecksFabricDestinationWithoutReplacingIt(t *testing.T) {
	h := newWorkerSSHHarness(t)
	inv := inventory.Inventory{AgentUsername: "alice", AdvertiseAddress: "192.0.2.2", Interfaces: []inventory.Interface{{Name: "fabric0", Addresses: []string{"10.42.0.2/24"}}}}
	worker, err := h.svc.workerSSHInventory(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	inv.ProtocolFeatures = worker.ProtocolFeatures
	raw, _ := json.Marshal(inv)
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: "worker", Inventory: nullString(string(raw))}); err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Workloads[0].Env = map[string]string{"WORKER_IP": "10.42.0.2", "WORKER_USER": "operator"}
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		if command.ResolveOnly || command.Target != "operator@10.42.0.2" || !reflect.DeepEqual(command.Addresses, []string{"192.0.2.2", "10.42.0.2"}) {
			t.Fatalf("source fabric SSH destination changed or selected addresses were lost: %+v", command)
		}
		return workerssh.Result{Target: command.Target, Hostname: "10.42.0.2", Username: "operator"}, nil
	})
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()})
	if err != nil || !plan.Ready || plan.Upstream[0].Environment["WORKER_IP"] != "10.42.0.2" {
		t.Fatalf("source fabric destination was not preserved: %+v, %v", plan, err)
	}
}

func TestWorkerSSHRejectsSharedHeadAddressesAsWorkerDestinations(t *testing.T) {
	h := newWorkerSSHHarness(t)
	inv, err := h.svc.workerSSHInventory(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	inv.Interfaces = append(inv.Interfaces, inventory.Interface{Name: "bridge0", Addresses: []string{"192.0.2.1"}})
	raw, _ := json.Marshal(inv)
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: "worker", Inventory: nullString(string(raw))}); err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Workloads[0].Env = map[string]string{"WORKER_HOST": "head-alias"}
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		return workerssh.Result{Target: command.Target, Hostname: "192.0.2.1", Username: "alice"}, nil
	})
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()})
	if err != nil || plan.Ready || !strings.Contains(fmt.Sprint(plan.Diagnostics), "not selected worker") {
		t.Fatalf("SSH to the head was accepted as access to the selected worker: %+v, %v", plan, err)
	}
}

func TestWorkerSSHUsesConfiguredUserWhenSourceLeavesUserEmpty(t *testing.T) {
	h := newWorkerSSHHarness(t)
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Workloads[0].Env = map[string]string{"WORKER_IP": "192.0.2.2", "WORKER_USER": ""}
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		if command.Target != "192.0.2.2" {
			return workerssh.Result{}, fmt.Errorf("source expects its configured SSH user, not %q", command.Target)
		}
		return workerssh.Result{Target: command.Target, Hostname: "192.0.2.2", Username: "configured-account"}, nil
	})
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()})
	if err != nil || !plan.Ready || plan.Upstream[0].Environment["WORKER_USER"] != "" {
		t.Fatalf("blank source username lost its configured SSH account: %+v, %v", plan, err)
	}
}

func TestWorkerSSHCreateRechecksBeforeSideEffects(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", deviceSettingsManifest(t))
	request := PlanRequest{RecipeDigest: "ssh-recipe", Placements: upstreamPlacements()}
	plan, err := h.svc.Plan(context.Background(), request)
	if err != nil || !plan.Ready {
		t.Fatalf("initial preflight failed: %+v, %v", plan, err)
	}
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		if !command.ResolveOnly {
			return workerssh.Result{}, errors.New("Permission denied (publickey)")
		}
		return successfulWorkerSSH(command)
	})
	h.svc.downloads = &fakeAcquisition{service: h.svc, acquire: func(context.Context, downloads.PlanRequest, bool) error {
		t.Fatal("failed SSH preflight triggered acquisition")
		return nil
	}}
	_, err = h.svc.Create(context.Background(), CreateRequest{RecipeDigest: request.RecipeDigest, Placements: request.Placements, PlanDigest: plan.Digest})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("create did not reject failed fresh preflight: %v", err)
	}
	for _, table := range []string{"deployments", "runs", "leases"} {
		var count int
		if err := h.dbh.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed preflight left %s side effects: count=%d, err=%v", table, count, err)
		}
	}
	if len(h.nodes.workloadCommands()) != 0 || len(h.nodes.transferCommands()) != 0 {
		t.Fatal("failed preflight dispatched workload or transfer commands")
	}
}

func TestWorkerSSHDoesNotGateWorkloadsWithoutWorkerSSHBindings(t *testing.T) {
	for _, source := range []bool{false, true} {
		t.Run(fmt.Sprintf("source=%t", source), func(t *testing.T) {
			h := newUpstreamHarness(t)
			if !source {
				h.seedRecipe(t, "no-ssh", noArtifactManifest)
			} else {
				manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
				if err != nil {
					t.Fatal(err)
				}
				manifest.Compatibility.NodeCount = 1
				manifest.Workloads[0].Ranks = []int{0}
				manifest.Workloads[0].Env = nil
				seedDeviceSettingsRecipe(t, h, "no-ssh", manifest)
			}
			plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "no-ssh", Placements: []PlacementOverride{{NodeID: "head", Rank: 0}}})
			if err != nil || !plan.Ready {
				t.Fatalf("no-SSH workload required an SSH broker/feature: %+v, %v", plan, err)
			}
		})
	}
}

func TestWorkerSSHRestartChecksFrozenDestinationBeforeSideEffects(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Workloads[0].Env = map[string]string{"WORKER_IP": "${cluster.node.1.address}", "WORKER_USER": "alice"}
	seedDeviceSettingsRecipe(t, h, "ssh-recipe", manifest)
	installWorkerSSHResponder(t, h, func(command *agentv1.WorkerSSHCommand) (workerssh.Result, error) {
		user, host, _ := strings.Cut(command.Target, "@")
		return workerssh.Result{Target: command.Target, Hostname: host, Username: user}, nil
	})
	ctx := context.Background()
	deployment := h.createDeployment(t, "ssh-recipe", upstreamPlacements()...)
	if _, err := h.svc.Stop(ctx, deployment.ID); err != nil {
		t.Fatal(err)
	}
	for rank, node := range []string{"head", "worker"} {
		h.svc.OnStateUpdate(ctx, node, &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: int32(rank), State: "missing"})
	}
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "198.51.100.2", nil)
	h.svc.downloads = &fakeAcquisition{service: h.svc, acquire: func(context.Context, downloads.PlanRequest, bool) error {
		t.Fatal("frozen SSH failure triggered acquisition")
		return nil
	}}
	before := deploymentRow(t, h, deployment.ID)
	commandsBefore := len(h.nodes.workloadCommands())
	_, err = h.svc.Start(ctx, deployment.ID)
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "not selected worker") {
		t.Fatalf("restart checked only fresh destination instead of frozen launch: %v", err)
	}
	after := deploymentRow(t, h, deployment.ID)
	if after.RunID != before.RunID || after.DesiredState != before.DesiredState || after.Placement != before.Placement || len(h.nodes.workloadCommands()) != commandsBefore {
		t.Fatal("failed frozen-target preflight changed persisted launch or dispatched a workload")
	}
	leases, err := h.q.ActiveLeases(ctx)
	if err != nil || len(leases) != 0 {
		t.Fatalf("failed frozen-target preflight acquired leases: %v, %v", leases, err)
	}
}
