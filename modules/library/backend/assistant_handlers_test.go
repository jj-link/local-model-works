package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
	"github.com/jj-link/local-model-works/internal/settings"
)

func assistantCatalogFixture(t *testing.T) (*Module, *sql.DB, *recipebuilder.Draft) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	q := db.New(database)
	registry := settings.New(q)
	if err := registry.Register("library", nil); err != nil {
		t.Fatal(err)
	}
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	builder := recipebuilder.New(q, root, validator, nil)
	builder.SetDB(database)
	draft, err := builder.Allocate(ctx, recipebuilder.GitSource{Remote: "https://github.com/example/repo"})
	if err != nil {
		t.Fatal(err)
	}
	seedInspectedGenerationSource(t, database, root, draft.ID)
	return &Module{env: &moduleapi.Env{Q: q, DB: database, Settings: registry, RecipeBuilder: builder}}, database, draft
}

func catalogResponse(t *testing.T, module *Module) (string, []assistantProviderConfig) {
	t.Helper()
	response := httptest.NewRecorder()
	Handler(module).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/recipe-assistant/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("catalog: HTTP %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "api_key_secret_id") {
		t.Fatalf("catalog exposed credential reference: %s", response.Body.String())
	}
	var catalog struct {
		Version   string                    `json:"version"`
		Providers []assistantProviderConfig `json:"providers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	return catalog.Version, catalog.Providers
}

func generationResponse(t *testing.T, module *Module, draft *recipebuilder.Draft, action string, input generationSelection) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/recipe-drafts/"+draft.ID+"/"+action, strings.NewReader(string(raw)))
	request.Header.Set("If-Match", `"`+strconv.FormatInt(draft.Version, 10)+`"`)
	response := httptest.NewRecorder()
	Handler(module).ServeHTTP(response, request)
	return response
}

func TestDirectLocalCatalogPreviewBindsLiveEndpointWithoutSavedProfiles(t *testing.T) {
	module, database, draft := assistantCatalogFixture(t)
	ctx := context.Background()
	q := module.env.Q
	module.env.Deploy = deploy.New(database, q, nil, nil, repositoryAPINodes{})
	if err := q.CreateNode(ctx, db.CreateNodeParams{ID: "enrolled", DisplayName: "Device", Labels: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetNodeStatus(ctx, db.SetNodeStatusParams{ID: "enrolled", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	insertAPIRecipe(t, ctx, q, digest, "local-model", "https://github.com/example/model", strings.Repeat("a", 40))
	if err := q.CreateDeployment(ctx, db.CreateDeploymentParams{ID: "serving", RecipeDigest: digest, Parameters: "{}", Placement: `{"entries":[{"node_id":"enrolled","rank":0}]}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE deployments SET observed_state='healthy',endpoint='127.0.0.1:8000',endpoint_model='local-model' WHERE id='serving'`); err != nil {
		t.Fatal(err)
	}
	version, providers := catalogResponse(t, module)
	if len(providers) != 1 || providers[0].ID != "local:serving" || providers[0].Model != "local-model" {
		t.Fatalf("direct deployment unavailable without profiles: %+v", providers)
	}
	selection := generationSelection{ProviderID: providers[0].ID, ProviderVersion: version, Model: providers[0].Model, DiagnosticIDs: []string{}, RunExcerpts: []runExcerptSelection{}}
	preview := generationResponse(t, module, draft, "generation-preview", selection)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: HTTP %d: %s", preview.Code, preview.Body.String())
	}
	var approval struct {
		PreviewSHA256 string `json:"preview_sha256"`
		Destination   string `json:"destination"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &approval); err != nil {
		t.Fatal(err)
	}
	if approval.Destination != "http://127.0.0.1:8000/v1" {
		t.Fatalf("unexpected destination: %q", approval.Destination)
	}
	selection.ContentConsent, selection.PreviewSHA256 = true, approval.PreviewSHA256
	if _, err := database.Exec(`UPDATE deployments SET endpoint='127.0.0.1:8001' WHERE id='serving'`); err != nil {
		t.Fatal(err)
	}
	if response := generationResponse(t, module, draft, "generate", selection); response.Code != http.StatusPreconditionFailed {
		t.Fatalf("changed endpoint accepted: HTTP %d: %s", response.Code, response.Body.String())
	}
	selection.Model = "another-model"
	if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("model override accepted: HTTP %d: %s", response.Code, response.Body.String())
	}
	selection.Model = "local-model"
	if err := q.SetNodeStatus(ctx, db.SetNodeStatusParams{ID: "enrolled", Status: "offline"}); err != nil {
		t.Fatal(err)
	}
	_, providers = catalogResponse(t, module)
	if len(providers) != 0 {
		t.Fatalf("offline provider advertised: %+v", providers)
	}
	if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusNotFound {
		t.Fatalf("unavailable selection accepted: HTTP %d: %s", response.Code, response.Body.String())
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := module.assistantProviders(ctx, nil); err == nil {
		t.Fatal("database discovery failure became an empty catalog")
	}
}

func TestDirectCodexCatalogRejectsUnavailableModelsWithoutSavedProfiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Codex executable fixture requires Unix shebang support")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Codex executable fixture requires python3")
	}
	module, _, draft := assistantCatalogFixture(t)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.WriteFile(state, []byte("connected"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "codex")
	script := "#!" + python + "\nimport json,sys\nstate=" + strconv.Quote(state) + `
for line in sys.stdin:
    request=json.loads(line)
    if 'id' not in request: continue
    method=request['method']
    mode=open(state).read()
    result={}
    if method=='account/read': result={'account': {'type':'chatgpt'} if mode!='disconnected' else None}
    elif method=='model/list': result={'data': [] if mode=='removed' else [{'id':'codex-model','displayName':'Codex Model'}]}
    elif method!='initialize': sys.exit(2)
    response={'id':request['id'],'result':result}
    if mode=='failed' and method=='model/list': response={'id':request['id'],'error':{'code':-1,'message':'model discovery failed'}}
    print(json.dumps(response),flush=True)
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	module.env.RecipeAssistant = recipeassistant.NewCodex(ctx, binary, root)
	version, providers := catalogResponse(t, module)
	if len(providers) != 1 || providers[0].ID != "codex:codex-model" {
		t.Fatalf("connected account unavailable without profiles: %+v", providers)
	}
	selection := generationSelection{ProviderID: providers[0].ID, ProviderVersion: version, Model: providers[0].Model, DiagnosticIDs: []string{}, RunExcerpts: []runExcerptSelection{}}
	if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusOK {
		t.Fatalf("Codex preview: HTTP %d: %s", response.Code, response.Body.String())
	}
	connection := httptest.NewRecorder()
	Handler(module).ServeHTTP(connection, httptest.NewRequest(http.MethodPost, "/recipe-assistant/providers/codex%3Acodex-model/test", nil))
	if connection.Code != http.StatusOK {
		t.Fatalf("direct Codex connection test: HTTP %d: %s", connection.Code, connection.Body.String())
	}
	selection.Model = "not-advertised"
	if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown Codex model accepted: HTTP %d: %s", response.Code, response.Body.String())
	}
	selection.Model = "codex-model"
	for _, mode := range []string{"removed", "disconnected"} {
		if err := os.WriteFile(state, []byte(mode), 0o600); err != nil {
			t.Fatal(err)
		}
		_, providers = catalogResponse(t, module)
		if len(providers) != 0 {
			t.Fatalf("%s model advertised: %+v", mode, providers)
		}
		if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusNotFound {
			t.Fatalf("%s selection accepted: HTTP %d: %s", mode, response.Code, response.Body.String())
		}
	}
	if err := os.WriteFile(state, []byte("failed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := module.assistantProviders(ctx, nil); err == nil {
		t.Fatal("Codex discovery failure became an empty catalog")
	}
}

func TestProviderCatalogKeepsRemoteConfigurationButIgnoresSavedLocalAndCodexProfiles(t *testing.T) {
	module, _, draft := assistantCatalogFixture(t)
	ctx := context.Background()
	stored := map[string]any{"assistant": map[string]any{
		"default_provider_id": "old-local",
		"providers": []any{
			map[string]any{"id": "old-local", "kind": "local", "deployment_id": "no-longer-serving"},
			map[string]any{"id": "old-codex", "kind": "codex", "model": "no-longer-available"},
			map[string]any{"id": "remote", "kind": "openai_compatible", "label": "Remote", "base_url": "https://api.example.com/v1", "model": "remote-model", "api_key_secret_id": "private-secret-reference"},
		},
	}}
	version, err := module.env.Settings.Set(ctx, "library", stored, "0")
	if err != nil {
		t.Fatal(err)
	}
	catalogVersion, providers := catalogResponse(t, module)
	if catalogVersion != version || len(providers) != 1 || providers[0].ID != "remote" || providers[0].APIKeySecretID != "" {
		t.Fatalf("catalog leaked saved profiles or lost remote endpoint: %q %+v", catalogVersion, providers)
	}
	selection := generationSelection{ProviderID: "old-codex", ProviderVersion: version, DiagnosticIDs: []string{}, RunExcerpts: []runExcerptSelection{}}
	if response := generationResponse(t, module, draft, "generation-preview", selection); response.Code != http.StatusNotFound {
		t.Fatalf("obsolete profile still resolves: HTTP %d: %s", response.Code, response.Body.String())
	}
	for _, reserved := range []string{"local:missing", "codex:missing"} {
		spoofed := map[string]any{"assistant": map[string]any{"providers": []any{map[string]any{
			"id": reserved, "kind": "openai_compatible", "base_url": "https://api.example.com/v1", "model": "remote-model",
		}}}}
		if _, err := module.assistantProviders(ctx, spoofed); err == nil {
			t.Fatalf("remote endpoint impersonated %s", reserved)
		}
	}
}
