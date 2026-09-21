package deploy

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

func TestInstallationUpdateRebindsTargetOffsetsWithSavedInputs(t *testing.T) {
	h := newUpstreamHarness(t)
	ctx := context.Background()
	deployment := h.createDeployment(t, "recipe-upstream", upstreamPlacements()...)
	waitAcquisition(t, h.svc)
	row := deploymentRow(t, h, deployment.ID)
	placement := ParsePlacementSet(row.Placement)
	for i := range placement.Upstream {
		placement.Upstream[i].Configuration = []sourceconfig.ResolvedFile{{Path: "old.sh", SHA256: strings.Repeat("1", 64), Edits: []sourceconfig.ResolvedEdit{{Start: 0, End: 3, Replacement: "obsolete"}}}}
	}
	if _, err := h.svc.db.ExecContext(ctx, "UPDATE deployments SET parameters = ?, placement = ? WHERE id = ?", `{"context_length":8192}`, placement.Marshal(), deployment.ID); err != nil {
		t.Fatal(err)
	}
	// A later inventory refresh cannot replace the installation's frozen peer.
	setUpstreamNodeInventory(t, h, "worker", "198.51.100.2", true)
	target, err := recipe.Parse([]byte(upstreamClusterManifest))
	if err != nil {
		t.Fatal(err)
	}
	target.Metadata.Source.Revision = strings.Repeat("b", 40)
	target.Parameters = []recipe.Parameter{{Name: "context_length", Type: "int", Default: 4096}}
	target.Workloads[0].Upstream.Configuration = []sourceconfig.File{{Path: "new.sh", SHA256: strings.Repeat("2", 64), Edits: []sourceconfig.Edit{{Start: 10, End: 14, Parameter: "context_length", Format: "shell"}}}}
	doc, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	h.seedRecipe(t, digest, string(doc))
	before := len(h.nodes.workloadCommands())
	inputs, err := h.svc.installationUpdateSpecs(ctx, "head", digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("source installation count = %d", len(inputs))
	}
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(inputs[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Env, "PEER=192.0.2.2") {
		t.Fatalf("lost frozen peer: %v", spec.Env)
	}
	files := spec.Upstream.Configuration
	if len(files) != 1 || files[0].Path != "new.sh" || files[0].SHA256 != strings.Repeat("2", 64) || len(files[0].Edits) != 1 || files[0].Edits[0].Start != 10 || files[0].Edits[0].End != 14 || !strings.Contains(files[0].Edits[0].Replacement, "8192") {
		t.Fatalf("target configuration was not freshly resolved using saved value: %+v", files)
	}
	if spec.Upstream.Revision != strings.Repeat("b", 40) {
		t.Fatal("old source revision retained")
	}
	if after := len(h.nodes.workloadCommands()); after != before {
		t.Fatal("reconstruction dispatched workload commands")
	}
	if got := deploymentRow(t, h, deployment.ID); got.RecipeDigest != row.RecipeDigest || got.DesiredState != row.DesiredState {
		t.Fatal("reconstruction changed running deployment")
	}
}

func TestInstallationUpdateRequiresCapableAgentWithoutInventingLegacySource(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedNode(t, "node-a", nil, "")
	oldDigest := "sha256:" + strings.Repeat("a", 64)
	targetDigest := "sha256:" + strings.Repeat("b", 64)
	h.seedRecipe(t, oldDigest, noArtifactManifest)
	repositoryID := "https://fixtures.local/package-only\n."
	seedRepositoryVersion(t, h, repositoryID, oldDigest, strings.Repeat("1", 40), true)
	h.seedRecipeUnplaced(t, targetDigest, upstreamClusterManifest)
	seedRepositoryVersion(t, h, repositoryID, targetDigest, strings.Repeat("2", 40), true)
	plan, err := h.svc.PlanRepositoryInstallationUpdate(ctx, repositoryID, targetDigest)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready {
		t.Fatal("unsupported agent passed confirmation gate")
	}
	if _, err := h.svc.CreateRepositoryInstallationUpdate(ctx, repositoryID, targetDigest, plan.Digest); err == nil {
		t.Fatal("unsupported agent update started")
	}
	node, err := h.q.GetNode(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal([]byte(node.Inventory.String), &inv); err != nil {
		t.Fatal(err)
	}
	inv.ProtocolFeatures = append(inv.ProtocolFeatures, recipe.InstallationUpdateProtocolFeature)
	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: node.ID, Inventory: nullString(string(raw))}); err != nil {
		t.Fatal(err)
	}
	plan, err = h.svc.PlanRepositoryInstallationUpdate(ctx, repositoryID, targetDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.UpToDate {
		t.Fatalf("supported package update not reviewable: %+v", plan)
	}
	if specs, ok := plan.installationSpecs["node-a"]; !ok || specs == nil || len(specs) != 0 {
		t.Fatalf("legacy native installation gained invented source configuration: %+v", specs)
	}
	if len(h.nodes.artifactCommands()) != 0 || len(h.nodes.workloadCommands()) != 0 {
		t.Fatal("review sent commands")
	}
}
