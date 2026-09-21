package recipebuilder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestResolvedLatestImageBecomesValidatedImmutableReference(t *testing.T) {
	service, draft := foundationDraft(t)
	ctx := context.Background()
	if _, err := service.db.ExecContext(ctx, `UPDATE recipe_drafts SET resolved_commit=? WHERE id=?`, strings.Repeat("a", 40), draft.ID); err != nil {
		t.Fatal(err)
	}
	manifest := json.RawMessage(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"pinning-regression","version":"1.0.0","description":"Reference pinning regression","license":"MIT"},"compatibility":{"nodeCount":1},"artifacts":[],"workloads":[{"image":{"reference":"registry.example/team/model:latest"},"command":["serve"],"args":[],"resources":{"pids":64}}]}`)
	updated, err := service.Update(ctx, draft.ID, draft.Version, UpdateRequest{Manifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := service.ReserveOperation(ctx, draft.ID, updated.Version, PhaseResolve)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	resolver := &ReferenceResolver{
		validateHost: func(context.Context, string) error { return nil },
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Docker-Content-Digest": []string{digest}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	resolved, err := service.ResolveReferences(ctx, draft.ID, reserved.Operation.ID, "", ResolveRequest{}, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := recipe.Parse(resolved.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != "valid" || parsed.Workloads[0].Image.Reference != "registry.example/team/model@"+digest {
		t.Fatalf("verified image remained mutable or unpackageable: state=%s image=%+v diagnostics=%+v", resolved.State, parsed.Workloads[0].Image, resolved.Diagnostics)
	}
}

func TestRepositoryInvestigationIncludesBuildAndDevcontainerDefinitions(t *testing.T) {
	service, _ := foundationDraft(t)
	root := filepath.Join(filepath.Dir(service.root), "build-definitions")
	files := map[string]string{
		".github/workflows/build.yml": "name: upstream image build\n",
		".devcontainer/Dockerfile":    "FROM upstream/runtime:documented\n",
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	draft, err := service.CreateFromDir(context.Background(), GitSource{Remote: "https://github.com/example/build-definitions"}, strings.Repeat("a", 40), strings.Repeat("b", 40), json.RawMessage(`{}`), root)
	if err != nil {
		t.Fatal(err)
	}
	request, err := service.GenerationRequest(context.Background(), draft.ID, draft.Version, "", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range request.SourceInventory {
		if content, exists := files[source.Path]; exists && source.Readable {
			_, retained, err := service.ReadOwnedFile(context.Background(), draft.ID, source.Path, source.SHA256, draft.ResolvedCommit)
			if err != nil || string(retained) != content {
				t.Fatalf("build definition lost its source bytes: %s %v", source.Path, err)
			}
			delete(files, source.Path)
		}
	}
	if len(files) != 0 {
		t.Fatalf("automatic investigation omitted launch/build evidence: %v", files)
	}
}
