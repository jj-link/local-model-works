package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type repositoryAPINodes struct{}

func (repositoryAPINodes) Send(string, *agentv1.ServerMessage) bool { return true }
func (repositoryAPINodes) Online(string) bool                       { return true }

func TestRepositoryDetailDeduplicatesVersionsAndShowsAffectedHardware(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	bus := events.NewEventBus(queries)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.New(database, queries, bus, validator, "", t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificateAuthority, err := ca.New()
	if err != nil {
		t.Fatal(err)
	}
	runsService := runs.New(database, queries, bus, t.TempDir())
	deployments := deploy.New(database, queries, bus, runsService, repositoryAPINodes{}, certificateAuthority)
	module := &Module{env: &moduleapi.Env{Ctx: ctx, DB: database, Q: queries, Bus: bus, Recipes: recipes, Deploy: deployments, Runs: runsService}}

	if err := queries.CreateNode(ctx, db.CreateNodeParams{ID: "node-a", DisplayName: "Spark A", Labels: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "online", ID: "node-a"}); err != nil {
		t.Fatal(err)
	}
	remote := "https://github.com/Acme/Recipe"
	oldCommit, newCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	oldDigest, newDigest := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	insertAPIRecipe(t, ctx, queries, oldDigest, "legacy-name", remote, oldCommit)
	insertAPIRecipe(t, ctx, queries, newDigest, "new-name", remote, newCommit)
	repositoryID, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: remote, Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := queries.UpsertRecipeRepository(ctx, db.UpsertRecipeRepositoryParams{
		ID: repositoryID, SourceUrl: remote, SourcePath: ".", TrackingRef: "main", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, version := range []struct{ digest, commit string }{{oldDigest, oldCommit}, {newDigest, newCommit}} {
		if err := queries.AttachRecipeRepositoryVersion(ctx, db.AttachRecipeRepositoryVersionParams{
			RepositoryID: repositoryID, RecipeDigest: version.digest, CommitSha: version.commit,
			Canonical: 1, InstalledAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queries.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
		CurrentDigest: sql.NullString{String: newDigest, Valid: true}, UpdatedAt: now, ID: repositoryID,
	}); err != nil {
		t.Fatal(err)
	}
	placement := `{"ranks":{"node-a":0},"entries":[{"node_id":"node-a","node_name":"Spark A","rank":0,"accelerator_index":0}],"workload":0}`
	if err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: "deployment-old", RecipeDigest: oldDigest, Profile: "", Placement: placement,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{ObservedState: "healthy", ID: "deployment-old"}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest("GET", "/recipe-repositories/"+repositoryID, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", repositoryID)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	response := httptest.NewRecorder()
	module.getRecipeRepository(response, request)
	if response.Code != 200 {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		ID               string                     `json:"id"`
		UpdateAvailable  bool                       `json:"update_available"`
		Versions         []recipe.RepositoryVersion `json:"versions"`
		AffectedHardware []affectedHardwareView     `json:"affected_hardware"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != repositoryID || len(body.Versions) != 2 {
		t.Fatalf("repository = %#v", body)
	}
	if !body.UpdateAvailable || len(body.AffectedHardware) != 1 {
		t.Fatalf("affected repository = %#v", body)
	}
	hardware := body.AffectedHardware[0]
	if hardware.NodeName != "Spark A" || len(hardware.DeploymentIDs) != 1 || hardware.DeploymentIDs[0] != "deployment-old" || hardware.State != "healthy" {
		t.Fatalf("hardware = %#v", hardware)
	}
}

func insertAPIRecipe(t *testing.T, ctx context.Context, queries *db.Queries, digest, name, remote, commit string) {
	t.Helper()
	manifest := map[string]any{
		"apiVersion": "localmodelworks/v1alpha1", "kind": "Recipe",
		"metadata": map[string]any{
			"name": name, "version": "1.0.0", "source": map[string]any{"url": remote, "path": ".", "revision": commit},
		},
		"compatibility": map[string]any{"nodeCount": 1}, "artifacts": []any{},
		"workloads": []any{map[string]any{
			"image":   map[string]any{"reference": "example@sha256:" + strings.Repeat("f", 64), "digest": "sha256:" + strings.Repeat("f", 64)},
			"command": []string{"serve"}, "args": []string{},
			"resources": map[string]any{"cpu": 1, "memoryBytes": 16777216, "pids": 64},
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: name, Version: "1.0.0", Source: `{"type":"local"}`,
		TrustState: recipe.TrustLocal, Manifest: string(encoded),
	}); err != nil {
		t.Fatal(err)
	}
}
