package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/moduleapi"
)

func TestLaunchProfilesRoundTripEncodedRecipeAndProfilePaths(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	digest := "sha256:" + strings.Repeat("1", 64)
	manifest := `{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"profile-settings","version":"1.0.0"},"parameters":[{"name":"context_length","type":"int","default":262144},{"name":"memory_fraction","type":"float","default":0.9}]}`
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: "profile-settings", Version: "1.0.0", Source: "{}", Manifest: manifest,
	}); err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{Deploy: deploy.New(database, queries, nil, nil, nil)}}
	handler := Handler(module)
	request := func(method, path, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		return response
	}
	path := "/recipes/sha256%3A" + strings.Repeat("1", 64) + "/launch-profiles"
	created := request(http.MethodPost, path, `{"name":"custom","parameters":{"context_length":131072,"memory_fraction":0.6}}`, http.StatusCreated)
	var profile deploy.LaunchProfile
	if err := json.Unmarshal(created.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.RecipeDigest != digest || profile.Parameters["context_length"] != float64(131072) || profile.Parameters["memory_fraction"] != 0.6 {
		t.Fatalf("saved profile does not belong to the requested recipe/settings: %+v", profile)
	}
	listed := request(http.MethodGet, path, "", http.StatusOK)
	var profiles []deploy.LaunchProfile
	if err := json.Unmarshal(listed.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].ID != profile.ID || profiles[0].Parameters["context_length"] != float64(131072) {
		t.Fatalf("saved profile not available through its encoded recipe URL: %+v", profiles)
	}
	profilePath := "/launch-profiles/" + strings.ReplaceAll(profile.ID, "-", "%2D")
	updated := request(http.MethodPut, profilePath, `{"name":"custom","parameters":{"context_length":65536}}`, http.StatusOK)
	var replacement deploy.LaunchProfile
	if err := json.Unmarshal(updated.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.ID != profile.ID || replacement.RecipeDigest != digest || replacement.Parameters["context_length"] != float64(65536) {
		t.Fatalf("encoded profile update changed identity or lost settings: %+v", replacement)
	}
	if _, retained := replacement.Parameters["memory_fraction"]; retained {
		t.Fatal("removed profile override was retained")
	}
	request(http.MethodDelete, profilePath, "", http.StatusNoContent)
	listed = request(http.MethodGet, path, "", http.StatusOK)
	if err := json.Unmarshal(listed.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("deleted profile remains visible: %+v", profiles)
	}
}
