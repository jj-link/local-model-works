package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type repositoryAPINodes struct{}

func (repositoryAPINodes) Send(string, *agentv1.ServerMessage) bool { return true }
func (repositoryAPINodes) Online(string) bool                       { return true }

func TestEncodedRecipeDigestUsesMountedDecodedParameter(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.New(database, queries, events.NewEventBus(queries), validator, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	insertAPIRecipe(t, ctx, queries, digest, "encoded-recipe", "https://example.invalid/recipe", strings.Repeat("a", 40))
	module := &Module{env: &moduleapi.Env{Ctx: ctx, Recipes: recipes}}
	response := httptest.NewRecorder()
	Handler(module).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/recipes/sha256%3A"+strings.Repeat("1", 64), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("encoded recipe lookup: HTTP %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Digest != digest {
		t.Fatalf("resolved recipe = %q, want %q", result.Digest, digest)
	}
}

func TestRepositoryDetailDeduplicatesVersionsAndShowsInstalledDevices(t *testing.T) {
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
	recipes, err := recipe.New(database, queries, bus, validator, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runsService := runs.New(database, queries, bus, t.TempDir())
	deployments := deploy.New(database, queries, bus, runsService, repositoryAPINodes{})
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
	repository, err := recipes.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.InstalledDevices) != 0 {
		t.Fatalf("repository without package placements is installed on %+v", repository.InstalledDevices)
	}
	if err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: "recipe-package-old", Kind: "recipe", Identity: "recipe://" + oldDigest,
		Digest: sql.NullString{String: oldDigest, Valid: true}, Metadata: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertPlacement(ctx, db.UpsertPlacementParams{
		ArtifactID: "recipe-package-old", NodeID: "node-a", Path: "/var/lib/lmw/recipes/old",
		State: "valid", Diagnostics: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: "recipe-package-new", Kind: "recipe", Identity: "recipe://" + newDigest,
		Digest: sql.NullString{String: newDigest, Valid: true}, Metadata: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertPlacement(ctx, db.UpsertPlacementParams{
		ArtifactID: "recipe-package-new", NodeID: "node-a", Path: "/var/lib/lmw/recipes/new",
		State: "valid", Diagnostics: "[]",
	}); err != nil {
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
		ID               string                             `json:"id"`
		Versions         []recipe.RepositoryVersion         `json:"versions"`
		InstalledDevices []recipe.RepositoryInstalledDevice `json:"installed_devices"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != repositoryID || len(body.Versions) != 2 {
		t.Fatalf("repository = %#v", body)
	}
	if len(body.InstalledDevices) != 1 {
		t.Fatalf("installed devices = %+v", body.InstalledDevices)
	}
	device := body.InstalledDevices[0]
	if device.NodeID != "node-a" || device.NodeName != "Spark A" ||
		len(device.InstalledDigests) != 2 ||
		device.InstalledDigests[0] != oldDigest || device.InstalledDigests[1] != newDigest {
		t.Fatalf("installed device = %+v", device)
	}
}

func TestDraftHandlersExposeETagAndMutationPreconditions(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(state, "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	builder := recipebuilder.New(queries, state, validator, nil)
	builder.SetDB(database)
	draft, err := builder.Allocate(ctx, recipebuilder.GitSource{Remote: "https://github.com/example/repository"})
	if err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{Ctx: ctx, DB: database, Q: queries, RecipeBuilder: builder}}
	withID := func(request *http.Request) *http.Request {
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("id", draft.ID)
		return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	}

	getResponse := httptest.NewRecorder()
	module.getDraft(getResponse, withID(httptest.NewRequest(http.MethodGet, "/recipe-drafts/"+draft.ID, nil)))
	if getResponse.Code != http.StatusOK || getResponse.Header().Get("ETag") != `"1"` {
		t.Fatalf("get status=%d etag=%q body=%s", getResponse.Code, getResponse.Header().Get("ETag"), getResponse.Body)
	}

	deleteResponse := httptest.NewRecorder()
	module.deleteDraft(deleteResponse, withID(httptest.NewRequest(http.MethodDelete, "/recipe-drafts/"+draft.ID, nil)))
	if deleteResponse.Code != http.StatusPreconditionRequired ||
		!strings.Contains(deleteResponse.Body.String(), "recipe.draft_if_match") {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body)
	}

	updateRequest := withID(httptest.NewRequest(http.MethodPut, "/recipe-drafts/"+draft.ID,
		strings.NewReader(`{"manifest":{},"selected_assets":[]}`)))
	updateRequest.Header.Set("If-Match", `"0"`)
	updateResponse := httptest.NewRecorder()
	module.updateDraft(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusPreconditionFailed ||
		!strings.Contains(updateResponse.Body.String(), "recipe.draft_stale_version") {
		t.Fatalf("update status=%d body=%s", updateResponse.Code, updateResponse.Body)
	}
}

func TestGenerationSetupFailureReleasesDraftForEditing(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(state, "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	builder := recipebuilder.New(queries, state, validator, nil)
	builder.SetDB(database)
	draft, err := builder.Allocate(ctx, recipebuilder.GitSource{Remote: "https://github.com/example/repository"})
	if err != nil {
		t.Fatal(err)
	}
	outbound, err := builder.GenerationRequest(ctx, draft.ID, draft.Version, "", "selected-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	approval := recipebuilder.GenerationApproval{DraftVersion: draft.Version, ProviderID: "unavailable", ProviderVersion: "1", ProviderKind: "codex", Destination: "codex://saved-account", Request: outbound}
	approval.PreviewSHA256, err = approval.Digest()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := builder.ReserveOperation(ctx, draft.ID, draft.Version, recipebuilder.PhaseGenerate)
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{env: &moduleapi.Env{RecipeBuilder: builder, Settings: settings.New(queries)}}
	_, err = m.generationJob(ctx, &jobs.Context{Input: map[string]any{
		"draft_id": draft.ID, "operation_id": reserved.Operation.ID,
		"approval": approval, "provider": assistantProviderConfig{ID: "unavailable", Kind: "codex", BaseURL: "codex://saved-account", Model: "selected-model"},
	}})
	if err == nil {
		t.Fatal("unavailable frozen provider must fail generation")
	}
	recovered, err := builder.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Operation != nil || recovered.State == "analyzing" {
		t.Fatalf("setup failure stranded editable work: state=%s operation=%+v", recovered.State, recovered.Operation)
	}
	if _, err := builder.ReserveOperation(ctx, draft.ID, recovered.Version, recipebuilder.PhaseGenerate); err != nil {
		t.Fatalf("operator cannot retry after setup failure: %v", err)
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
			"resources": map[string]any{"pids": 64},
		}},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: name, Version: "1.0.0", Source: `{"type":"local"}`,
		Manifest: string(encoded),
	}); err != nil {
		t.Fatal(err)
	}
}
