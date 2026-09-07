package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	"github.com/jj-link/local-model-works/internal/settings"
)

func TestGenerationPreviewRequiresFreshExplicitConsentWithoutProviderCalls(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer provider.Close()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	q := db.New(database)
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
	registry := settings.New(q)
	if err := registry.Register("library", nil); err != nil {
		t.Fatal(err)
	}
	providerSettings := map[string]any{"assistant": map[string]any{"providers": []any{map[string]any{"id": "chosen", "kind": "openai_compatible", "base_url": provider.URL, "model": "fixture"}}}}
	version, err := registry.Set(ctx, "library", providerSettings, "0")
	if err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{RecipeBuilder: builder, Settings: registry, Q: q}}
	router := chi.NewRouter()
	router.Post("/drafts/{id}/preview", module.previewRecipeGeneration)
	router.Post("/drafts/{id}/generate", module.generateDraft)
	selection := generationSelection{ProviderID: "chosen", ProviderVersion: version, DiagnosticIDs: []string{}, RunExcerpts: []runExcerptSelection{}}
	request := func(route string, input generationSelection) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", "/drafts/"+draft.ID+route, strings.NewReader(string(raw)))
		r.Header.Set("If-Match", `"`+strconv.FormatInt(draft.Version, 10)+`"`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	preview := request("/preview", selection)
	if preview.Code != 200 {
		t.Fatalf("preview failed: %d %s", preview.Code, preview.Body.String())
	}
	var result struct {
		PreviewSHA256 string `json:"preview_sha256"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rejected := request("/generate", selection); rejected.Code != 422 {
		t.Fatalf("missing consent accepted: %d %s", rejected.Code, rejected.Body.String())
	}
	selection.ContentConsent, selection.PreviewSHA256 = true, result.PreviewSHA256
	selection.Instruction = "changed after preview"
	if rejected := request("/generate", selection); rejected.Code != 412 {
		t.Fatalf("changed content accepted: %d %s", rejected.Code, rejected.Body.String())
	}
	selection.Instruction = ""
	nextVersion, err := registry.Set(ctx, "library", providerSettings, version)
	if err != nil {
		t.Fatal(err)
	}
	if rejected := request("/generate", selection); rejected.Code != 412 {
		t.Fatalf("changed provider settings accepted: %d %s", rejected.Code, rejected.Body.String())
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO secrets(id,name,purpose,nonce,ciphertext) VALUES('wrong-purpose','HF credential','huggingface',X'00',X'00')`); err != nil {
		t.Fatal(err)
	}
	providerSettings = map[string]any{"assistant": map[string]any{"providers": []any{map[string]any{"id": "chosen", "kind": "openai_compatible", "base_url": provider.URL, "model": "fixture", "api_key_secret_id": "wrong-purpose"}}}}
	selection.ProviderVersion, err = registry.Set(ctx, "library", providerSettings, nextVersion)
	if err != nil {
		t.Fatal(err)
	}
	if rejected := request("/preview", selection); rejected.Code != 400 || !strings.Contains(rejected.Body.String(), "assistant.secret_purpose") {
		t.Fatalf("unrelated credential accepted: %d %s", rejected.Code, rejected.Body.String())
	}
	current, err := builder.Get(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || current.Operation != nil || current.Version != draft.Version {
		t.Fatalf("preview or rejected consent performed work: provider calls=%d draft=%+v", calls.Load(), current)
	}
}

func TestSelectedGenerationLogsAreSanitizedWithoutHidingFailure(t *testing.T) {
	log := "\x1b[31mfailed loading weights\x1b[0m\nAuthorization: Bearer private-token\ncredential-value at rank 2\n\x1b]0;hidden title\x07"
	got := sanitizeGenerationLog(log, []string{"credential-value", "private-token"})
	if strings.Contains(got, "private-token") || strings.Contains(got, "credential-value") || strings.Contains(got, "\x1b") || strings.Contains(got, "hidden title") {
		t.Fatalf("unsafe excerpt: %q", got)
	}
	if !strings.Contains(got, "failed loading weights") || !strings.Contains(got, "rank 2") {
		t.Fatalf("failure evidence lost: %q", got)
	}
}
