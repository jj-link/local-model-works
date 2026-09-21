package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
)

func deviceSettingsManifest(t *testing.T) *recipe.Manifest {
	t.Helper()
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately unrelated parameter names: the authored environment binding
	// supplies the meaning, not a recipe ID or a conventional setting name.
	for _, name := range []string{"login", "target", "account", "home", "directory", "cache"} {
		manifest.Parameters = append(manifest.Parameters, recipe.Parameter{Name: name, Type: "string"})
	}
	manifest.Workloads[0].Env = map[string]string{
		"WORKER_SSH": "${setting.login}", "WORKER_HOST": "${setting.target}",
		"WORKER_USER": "${setting.account}", "WORKER_HOME": "${setting.home}",
		"WORKER_SCRIPT_DIR": "${setting.directory}", "WORKER_HF_CACHE": "${setting.cache}",
	}
	return manifest
}

func seedDeviceSettingsRecipe(t *testing.T, h *harness, digest string, manifest *recipe.Manifest) {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	h.seedRecipe(t, digest, string(raw))
}

func setDeviceSettingsInventory(t *testing.T, h *harness, nodeID, username, home, workspace, address string, roots []inventory.CacheRoot) {
	t.Helper()
	raw, err := json.Marshal(inventory.Inventory{
		Hostname: nodeID, AgentUsername: username, AgentHome: home, AgentWorkspace: workspace,
		AdvertiseAddress: address, CacheRoots: roots,
		ProtocolFeatures: []string{runtime.UpstreamProtocolFeature},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: nodeID, Inventory: nullString(string(raw))}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceDefaultsFollowSelectedWorkerAndPinnedSource(t *testing.T) {
	h := newWorkerSSHHarness(t)
	h.seedNode(t, "other-worker", nil, "")
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/alice/lmw", "192.0.2.2", []inventory.CacheRoot{{Path: "/models/huggingface", Backend: "huggingface", Writable: true}})
	setDeviceSettingsInventory(t, h, "other-worker", "bob", "/users/bob", "/srv/bob/lmw", "198.51.100.3", []inventory.CacheRoot{{Path: "/models", Backend: "local", Writable: true}})
	manifest := deviceSettingsManifest(t)
	seedDeviceSettingsRecipe(t, h, "device-recipe", manifest)
	ctx := context.Background()
	var firstDirectory string
	for _, worker := range []struct {
		id, account, home, workspace, address, cache string
	}{
		{"worker", "alice", "/home/alice", "/srv/alice/lmw", "192.0.2.2", "/models/huggingface"},
		{"other-worker", "bob", "/users/bob", "/srv/bob/lmw", "198.51.100.3", "/users/bob/.cache/huggingface"},
	} {
		req := PlanRequest{RecipeDigest: "device-recipe", Placements: []PlacementOverride{{NodeID: "head", Rank: 0}, {NodeID: worker.id, Rank: 1}}}
		plan, err := h.svc.Plan(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Ready {
			t.Fatalf("selected worker did not produce a ready plan: %+v", plan.Diagnostics)
		}
		for name, want := range map[string]string{
			"login": worker.account + "@" + worker.address, "target": worker.account + "@" + worker.address,
			"account": worker.account, "home": worker.home, "cache": worker.cache,
		} {
			if got := plan.Parameters[name]; got != want {
				t.Errorf("selected %s: %s = %v, want %s", worker.id, name, got, want)
			}
		}
		directory, ok := plan.Parameters["directory"].(string)
		if !ok || !strings.HasPrefix(directory, worker.workspace+"/worker-deployments/") || path.Clean(directory) != directory {
			t.Fatalf("worker directory escapes its reported workspace: %v", plan.Parameters["directory"])
		}
		if worker.id == "worker" {
			firstDirectory = directory
		}
		for key, binding := range manifest.Workloads[0].Env {
			name := strings.TrimSuffix(strings.TrimPrefix(binding, "${setting."), "}")
			if plan.Upstream[0].Environment[key] != plan.Parameters[name] {
				t.Fatalf("reviewed source environment differs from resolved setting %s", name)
			}
		}
		repeated, err := h.svc.Plan(ctx, req)
		if err != nil || repeated.Digest != plan.Digest {
			t.Fatalf("unchanged selected facts changed approval: %v", err)
		}
	}
	manifest.Metadata.Source.Revision = strings.Repeat("b", 40)
	seedDeviceSettingsRecipe(t, h, "new-source-revision", manifest)
	changed, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "new-source-revision", Placements: upstreamPlacements()})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Parameters["directory"] == firstDirectory {
		t.Fatal("different pinned revisions share the worker deployment directory")
	}
}

func TestDeviceDefaultsRejectStaleApprovalAfterInventoryChange(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	seedDeviceSettingsRecipe(t, h, "device-recipe", deviceSettingsManifest(t))
	ctx := context.Background()
	plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "device-recipe", Placements: upstreamPlacements()})
	if err != nil {
		t.Fatal(err)
	}
	setDeviceSettingsInventory(t, h, "worker", "renamed", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	_, err = h.svc.Create(ctx, CreateRequest{RecipeDigest: "device-recipe", Placements: upstreamPlacements(), PlanDigest: plan.Digest})
	if !errors.Is(err, ErrPlanStale) {
		t.Fatalf("changed account accepted previously reviewed worker defaults: %v", err)
	}
	if plan.Upstream[0].Environment["WORKER_SSH"] != "alice@192.0.2.2" {
		t.Fatal("existing preview was mutated by fresh inventory")
	}
}

func TestDeviceDefaultsPreserveOverridesDefaultsAndProfiles(t *testing.T) {
	h := newWorkerSSHHarness(t)
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/worker", "192.0.2.2", nil)
	manifest := deviceSettingsManifest(t)
	manifest.ParameterByName("home").Default = "/source-owned/home"
	seedDeviceSettingsRecipe(t, h, "device-recipe", manifest)
	ctx := context.Background()
	overrides := map[string]any{"login": "operator@ssh-alias"}
	plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "device-recipe", Placements: upstreamPlacements(), Parameters: overrides})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Parameters["login"] != "operator@ssh-alias" || plan.Parameters["home"] != "/source-owned/home" {
		t.Fatalf("inventory replaced authoritative launch values: %v", plan.Parameters)
	}
	if !reflect.DeepEqual(overrides, map[string]any{"login": "operator@ssh-alias"}) {
		t.Fatalf("planning turned implicit defaults into caller overrides: %v", overrides)
	}
	profile, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{Name: "portable", RecipeDigest: "device-recipe", Parameters: overrides})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(profile.Parameters, overrides) {
		t.Fatalf("saving a portable profile pinned device defaults: %v", profile.Parameters)
	}
	profile, err = h.svc.UpdateLaunchProfile(ctx, profile.ID, UpsertLaunchProfileRequest{Name: "portable updated", RecipeDigest: "device-recipe", Parameters: overrides})
	if err != nil {
		t.Fatal(err)
	}
	setDeviceSettingsInventory(t, h, "worker", "bob", "/home/bob", "/srv/bob", "192.0.2.3", nil)
	profilePlan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "device-recipe", Placements: upstreamPlacements(), LaunchProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	if profilePlan.Parameters["login"] != "operator@ssh-alias" || profilePlan.Parameters["account"] != "bob" || profilePlan.Parameters["home"] != "/source-owned/home" {
		t.Fatalf("profile pinned implicit values or lost authoritative values: %v", profilePlan.Parameters)
	}
	_, err = h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{Name: "invalid", RecipeDigest: "device-recipe", Parameters: map[string]any{"login": 42}})
	if !errors.Is(err, ErrProfile) {
		t.Fatalf("deferring required worker settings bypassed explicit value validation: %v", err)
	}
}

func TestDeviceDefaultsDoNotGuessMissingFactsOrUnrelatedSettings(t *testing.T) {
	for _, scenario := range []string{"missing-account", "automatic-placement", "unrelated-secret", "sensitive-binding", "indirect-binding", "missing-workspace", "unwritable-default-cache"} {
		t.Run(scenario, func(t *testing.T) {
			h := newWorkerSSHHarness(t)
			manifest := deviceSettingsManifest(t)
			username, workspace := "alice", "/srv/worker"
			var roots []inventory.CacheRoot
			req := PlanRequest{RecipeDigest: "device-recipe", Placements: upstreamPlacements()}
			switch scenario {
			case "missing-account":
				username = ""
			case "automatic-placement":
				req.Placements = nil
			case "unrelated-secret":
				manifest.Parameters = append(manifest.Parameters, recipe.Parameter{Name: "token", Type: "string", Sensitive: true})
				manifest.Workloads[0].Env["API_TOKEN"] = "${setting.token}"
			case "sensitive-binding":
				manifest.ParameterByName("login").Sensitive = true
			case "indirect-binding":
				manifest.Workloads[0].Env["WORKER_SSH"] = "prefix-${setting.login}"
			case "missing-workspace":
				workspace = ""
			case "unwritable-default-cache":
				roots = []inventory.CacheRoot{{Path: "/home/alice/.cache/huggingface", Backend: "huggingface", Writable: false}}
			}
			setDeviceSettingsInventory(t, h, "worker", username, "/home/alice", workspace, "192.0.2.2", roots)
			seedDeviceSettingsRecipe(t, h, "device-recipe", manifest)
			_, err := h.svc.Plan(context.Background(), req)
			if !errors.Is(err, ErrProfile) {
				t.Fatalf("missing or ineligible value did not fail validation: %v", err)
			}
			if scenario == "unrelated-secret" || scenario == "sensitive-binding" || scenario == "indirect-binding" {
				_, err = h.svc.CreateLaunchProfile(context.Background(), UpsertLaunchProfileRequest{Name: "incomplete", RecipeDigest: "device-recipe"})
				if !errors.Is(err, ErrProfile) {
					t.Fatalf("portable profile deferred an unrelated required value: %v", err)
				}
			}
		})
	}
}

func TestDeviceDefaultsResolveNumberedWorkerBindings(t *testing.T) {
	h := newWorkerSSHHarness(t)
	h.seedNode(t, "third", nil, "")
	setDeviceSettingsInventory(t, h, "worker", "alice", "/home/alice", "/srv/alice", "192.0.2.2", nil)
	setDeviceSettingsInventory(t, h, "third", "bob", "/home/bob", "/srv/bob", "192.0.2.3", nil)
	manifest := deviceSettingsManifest(t)
	manifest.Compatibility.NodeCount = 3
	manifest.Workloads[0].Ranks = []int{0, 1, 2}
	manifest.Parameters = append(manifest.Parameters, recipe.Parameter{Name: "second_target", Type: "string"})
	manifest.Workloads[0].Env["WORKER2_HOST"] = "${setting.second_target}"
	manifest.Workloads[0].Env["WORKER_2_SSH"] = "${setting.second_target}"
	seedDeviceSettingsRecipe(t, h, "device-recipe", manifest)
	plan, err := h.svc.Plan(context.Background(), PlanRequest{RecipeDigest: "device-recipe", Placements: append(upstreamPlacements(), PlacementOverride{NodeID: "third", Rank: 2})})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Parameters["target"] != "alice@192.0.2.2" || plan.Parameters["second_target"] != "bob@192.0.2.3" {
		t.Fatalf("numbered worker binding used the wrong explicit rank: %v", plan.Parameters)
	}
}
