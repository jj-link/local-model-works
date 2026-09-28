package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func hostCacheManifest(t *testing.T) *recipe.Manifest {
	t.Helper()
	manifest, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Parameters = []recipe.Parameter{{Name: "hf_home", Type: "string", Optional: true}}
	manifest.Workloads[0].Upstream.Configuration = []sourceconfig.File{{
		Path: "start.sh", SHA256: strings.Repeat("1", 64),
		Edits: []sourceconfig.Edit{{Start: 8, End: 40, Parameter: "hf_home", Format: "shell"}},
	}}
	return manifest
}

func setHostCacheInventory(t *testing.T, h *harness, nodeID, root string) {
	t.Helper()
	inv := inventory.Inventory{
		Hostname: nodeID, AgentHome: "/root", AdvertiseAddress: "192.0.2.1",
		CacheRoots:       []inventory.CacheRoot{{Path: root, Backend: "huggingface", Writable: true}},
		ProtocolFeatures: []string{runtime.UpstreamProtocolFeature, runtime.UpstreamConfigurationProtocolFeature},
	}
	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.SetNodeInventory(context.Background(), db.SetNodeInventoryParams{ID: nodeID, Inventory: nullString(string(data))}); err != nil {
		t.Fatal(err)
	}
}

func TestHostCacheDefaultUsesConfiguredRootAndBindsApproval(t *testing.T) {
	h := newUpstreamHarness(t)
	setHostCacheInventory(t, h, "head", "/models/shared HF")
	setHostCacheInventory(t, h, "worker", "/worker/different-cache")
	seedDeviceSettingsRecipe(t, h, "shared-cache", hostCacheManifest(t))
	req := PlanRequest{RecipeDigest: "shared-cache", Placements: upstreamPlacements()}
	plan, err := h.svc.Plan(context.Background(), req)
	if err != nil || !plan.Ready {
		t.Fatalf("configured host cache did not produce a ready plan: %+v, %v", plan, err)
	}
	if plan.Parameters["hf_home"] != "/models/shared HF" {
		t.Fatalf("launch defaults to an isolated checkout or agent home: %v", plan.Parameters)
	}
	repeated, err := h.svc.Plan(context.Background(), req)
	if err != nil || repeated.Digest != plan.Digest {
		t.Fatalf("unchanged shared cache changed approval: %v", err)
	}
	setHostCacheInventory(t, h, "head", "/models/new-cache")
	_, err = h.svc.Create(context.Background(), CreateRequest{RecipeDigest: req.RecipeDigest, Placements: req.Placements, PlanDigest: plan.Digest})
	if !errors.Is(err, ErrPlanStale) {
		t.Fatalf("cache mount changed after approval without a new review: %v", err)
	}
}

func TestHostCacheDefaultsPreserveExplicitSourceAndProfileValues(t *testing.T) {
	h := newUpstreamHarness(t)
	setHostCacheInventory(t, h, "head", "/models/shared")
	manifest := hostCacheManifest(t)
	seedDeviceSettingsRecipe(t, h, "shared-cache", manifest)
	ctx := context.Background()
	overrides := map[string]any{"hf_home": "/operator/cache"}
	profile, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{Name: "operator", RecipeDigest: "shared-cache", Parameters: overrides})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []PlanRequest{
		{RecipeDigest: "shared-cache", Placements: upstreamPlacements(), Parameters: overrides},
		{RecipeDigest: "shared-cache", Placements: upstreamPlacements(), LaunchProfileID: profile.ID},
	} {
		plan, err := h.svc.Plan(ctx, req)
		if err != nil || plan.Parameters["hf_home"] != "/operator/cache" {
			t.Fatalf("explicit cache setting was replaced: %+v, %v", plan, err)
		}
	}
	if !reflect.DeepEqual(overrides, map[string]any{"hf_home": "/operator/cache"}) {
		t.Fatal("planning mutated the caller's settings")
	}
	manifest.Parameters[0].Default = "/source/cache"
	seedDeviceSettingsRecipe(t, h, "source-cache", manifest)
	plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "source-cache", Placements: upstreamPlacements()})
	if err != nil || plan.Parameters["hf_home"] != "/source/cache" {
		t.Fatalf("explicit source default was replaced: %+v, %v", plan, err)
	}
}

func TestHostCacheDefaultsDoNotInventRootsOrInterpretUnrelatedSettings(t *testing.T) {
	for _, scenario := range []string{"no-configured-root", "hub-is-not-home", "no-placement", "different-executors", "conflicting-host-worker", "unrelated-name", "environment-binding"} {
		t.Run(scenario, func(t *testing.T) {
			h := newUpstreamHarness(t)
			manifest := hostCacheManifest(t)
			setHostCacheInventory(t, h, "head", "/models/shared")
			setHostCacheInventory(t, h, "worker", "/worker/cache")
			req := PlanRequest{RecipeDigest: "cache-case", Placements: upstreamPlacements()}
			wantError := true
			switch scenario {
			case "no-configured-root":
				setDeviceSettingsInventory(t, h, "head", "root", "/root", "/workspace", "192.0.2.1", nil)
			case "hub-is-not-home":
				setHostCacheInventory(t, h, "head", "/models/huggingface/hub")
			case "no-placement":
				req.Placements = nil
			case "different-executors":
				manifest.Workloads[0].Upstream.CoordinatorRank = nil
			case "conflicting-host-worker":
				manifest.Parameters[0].Optional = false
				manifest.Workloads[0].Env["WORKER_HF_CACHE"] = "${setting.hf_home}"
			case "unrelated-name":
				manifest.Workloads[0].Upstream.Configuration = nil
				wantError = false
			case "environment-binding":
				manifest.Parameters[0].Name = "model_store"
				manifest.Workloads[0].Upstream.Configuration = nil
				manifest.Workloads[0].Env["HF_CACHE"] = "${setting.model_store}"
				wantError = false
			}
			seedDeviceSettingsRecipe(t, h, "cache-case", manifest)
			plan, err := h.svc.Plan(context.Background(), req)
			if wantError {
				if !errors.Is(err, ErrProfile) {
					t.Fatalf("unsafe cache default was accepted: %+v, %v", plan, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unrelated-name" {
				if _, present := plan.Parameters["hf_home"]; present {
					t.Fatal("setting name alone was treated as a host cache binding")
				}
			} else if plan.Parameters["model_store"] != "/models/shared" {
				t.Fatalf("source-owned HF_CACHE binding did not reuse the shared root: %v", plan.Parameters)
			}
		})
	}
}

func TestHostCacheRestartRetainsPreviouslyOmittedOverride(t *testing.T) {
	h := newUpstreamHarness(t)
	setHostCacheInventory(t, h, "head", "/models/shared")
	setHostCacheInventory(t, h, "worker", "/worker/cache")
	seedDeviceSettingsRecipe(t, h, "shared-cache", hostCacheManifest(t))
	ctx := context.Background()
	deployment := h.createDeployment(t, "shared-cache", upstreamPlacements()...)
	if _, err := h.svc.Stop(ctx, deployment.ID); err != nil {
		t.Fatal(err)
	}
	for rank, node := range []string{"head", "worker"} {
		h.svc.OnStateUpdate(ctx, node, &agentv1.StateUpdate{DeploymentId: deployment.ID, RunId: deployment.RunID, Rank: int32(rank), State: "missing"})
	}
	// Model the durable contract saved before host-cache inference existed:
	// the optional override was absent and the original source owned its path.
	saved := ParsePlacementSet(deploymentRow(t, h, deployment.ID).Placement)
	for i := range saved.Upstream {
		saved.Upstream[i].Configuration = nil
	}
	placement, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.dbh.ExecContext(ctx, "UPDATE deployments SET parameters=json_remove(parameters, '$.hf_home'), placement=? WHERE id=?", string(placement), deployment.ID); err != nil {
		t.Fatal(err)
	}
	setUpstreamNodeInventory(t, h, "head", "192.0.2.1", true)
	if _, err := h.svc.Start(ctx, deployment.ID); err != nil {
		t.Fatalf("frozen restart imposed an unused new cache requirement: %v", err)
	}
	waitAcquisition(t, h.svc)
	row := deploymentRow(t, h, deployment.ID)
	if _, supplied := parametersForValue(row.Parameters)["hf_home"]; supplied {
		t.Fatal("restart injected a new cache override into saved inputs")
	}
	for _, preview := range ParsePlacementSet(row.Placement).Upstream {
		if len(preview.Configuration) != 0 {
			t.Fatal("restart replaced the frozen source cache assignment")
		}
	}
	if _, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "shared-cache", Placements: upstreamPlacements()}); !errors.Is(err, ErrProfile) {
		t.Fatalf("new launch bypassed the configured-cache requirement: %v", err)
	}
}

func TestRequiredHostCacheCanRemainPortableInProfiles(t *testing.T) {
	h := newUpstreamHarness(t)
	setHostCacheInventory(t, h, "head", "/models/shared")
	manifest := hostCacheManifest(t)
	manifest.Parameters = []recipe.Parameter{{Name: "model_store", Type: "string"}}
	manifest.Workloads[0].Upstream.Configuration = nil
	manifest.Workloads[0].Env["HF_HOME"] = "${setting.model_store}"
	seedDeviceSettingsRecipe(t, h, "portable-cache", manifest)
	ctx := context.Background()
	profile, err := h.svc.CreateLaunchProfile(ctx, UpsertLaunchProfileRequest{Name: "portable", RecipeDigest: "portable-cache"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err = h.svc.UpdateLaunchProfile(ctx, profile.ID, UpsertLaunchProfileRequest{Name: "portable updated", RecipeDigest: "portable-cache"})
	if err != nil || len(profile.Parameters) != 0 {
		t.Fatalf("profile pinned or required a device-local path: %+v, %v", profile, err)
	}
	setHostCacheInventory(t, h, "head", "/models/selected-later")
	plan, err := h.svc.Plan(ctx, PlanRequest{RecipeDigest: "portable-cache", Placements: upstreamPlacements(), LaunchProfileID: profile.ID})
	if err != nil || plan.Parameters["model_store"] != "/models/selected-later" {
		t.Fatalf("portable profile did not resolve the selected device cache: %+v, %v", plan, err)
	}
	_, err = h.svc.UpdateLaunchProfile(ctx, profile.ID, UpsertLaunchProfileRequest{Name: "invalid", RecipeDigest: "portable-cache", Parameters: map[string]any{"model_store": 42}})
	if !errors.Is(err, ErrProfile) {
		t.Fatalf("profile deferral bypassed validation of an explicit cache: %v", err)
	}
}
