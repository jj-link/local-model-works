package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestUpdatePackagePreservesSavedConfigurationAndAssets(t *testing.T) {
	baseDocument := `{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"saved","version":"1","source":{"url":"https://github.com/example/update","path":".","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},"workloads":[{"env":{"PORT":"8000","UPSTREAM":"old"},"command":["serve","--old"]}],"assets":["config.env","upstream.txt"]}`
	savedDocument := strings.Replace(baseDocument, `"PORT":"8000"`, `"PORT":"9000"`, 1)
	savedDocument = strings.Replace(savedDocument, `"version":"1"`, `"version":"local"`, 1)
	targetDocument := strings.Replace(baseDocument, `"UPSTREAM":"old"`, `"UPSTREAM":"new"`, 1)
	targetDocument = strings.Replace(targetDocument, `"--old"`, `"--new"`, 1)
	targetDocument = strings.Replace(targetDocument, `"version":"1"`, `"version":"2"`, 1)
	targetDocument = strings.Replace(targetDocument, strings.Repeat("a", 40), strings.Repeat("b", 40), 1)
	pack := func(document, config, upstream string) *PackResult {
		t.Helper()
		result, err := PackManifest([]byte(document), map[string][]byte{"config.env": []byte(config), "upstream.txt": []byte(upstream)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	merged, err := preserveSavedPackage(context.Background(), t.TempDir(), pack(baseDocument, "PORT=8000\nFEATURE=old\n", "old"), pack(savedDocument, "PORT=9000\nFEATURE=old\n", "old"), pack(targetDocument, "PORT=8000\nFEATURE=new\n", "new"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Parse(merged.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.Version != "2" || manifest.Metadata.Source.Revision != strings.Repeat("b", 40) {
		t.Fatalf("update retained obsolete provenance: %+v", manifest.Metadata)
	}
	if manifest.Workloads[0].Env["PORT"] != "9000" || manifest.Workloads[0].Env["UPSTREAM"] != "new" || manifest.Workloads[0].Command[1] != "--new" {
		t.Fatalf("saved configuration or upstream changes lost: %+v", manifest.Workloads[0])
	}
	assets, err := updatePackageAssets(merged)
	if err != nil {
		t.Fatal(err)
	}
	if string(assets["config.env"]) != "PORT=9000\nFEATURE=new\n" || string(assets["upstream.txt"]) != "new" {
		t.Fatalf("saved asset or upstream asset lost: %+v", assets)
	}
}

func TestUpdateRetainsNamedSettingsWhenUpstreamAddsAndReordersParameters(t *testing.T) {
	base := `{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"settings","version":"1"},"parameters":[{"name":"port","type":"int","default":8000},{"name":"context","type":"int","default":4096}],"workloads":[]}`
	saved := strings.Replace(base, `"default":8000`, `"default":9000`, 1)
	target := `{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"settings","version":"2"},"parameters":[{"name":"context","type":"int","default":8192},{"name":"batch","type":"int","default":32},{"name":"port","type":"int","default":8500}],"workloads":[]}`
	packages := make([]*PackResult, 0, 3)
	for _, document := range []string{base, saved, target} {
		packed, err := PackManifest([]byte(document), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		packages = append(packages, packed)
	}
	updated, err := preserveSavedPackage(context.Background(), t.TempDir(), packages[0], packages[1], packages[2])
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Parse(updated.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := manifest.EffectiveSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"port": "9000", "context": "8192", "batch": "32"} {
		value, ok := (RenderContext{Settings: settings}).Resolve("${setting." + name + "}")
		if !ok || value != want {
			t.Fatalf("updated setting %s = %q, %v; want %q", name, value, ok, want)
		}
	}
}

func TestCheckUpdatesCachesGitHubHeadAcrossRecipes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(database, queries, events.NewEventBus(queries), validator, filepath.Join(root, "catalog"), filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}

	installed := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	createUpdateRecipe(t, ctx, queries, "alpha", installed, "https://github.com/MiaAI-Lab/shared", "alpha")
	createUpdateRecipe(t, ctx, queries, "beta", candidate, "https://github.com/MiaAI-Lab/shared.git", "beta")
	createUpdateRecipe(t, ctx, queries, "local-only", installed, "https://fixtures.local/local-only", ".")

	resolveCalls := 0
	service.resolveGitHead = func(_ context.Context, remote string) (string, string, error) {
		resolveCalls++
		if remote != "https://github.com/MiaAI-Lab/shared" {
			t.Fatalf("normalized remote = %q", remote)
		}
		return "main", candidate, nil
	}

	statuses, err := service.CheckUpdatesNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolveCalls)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2: %+v", len(statuses), statuses)
	}
	byRevision := map[string]UpdateStatus{}
	for _, status := range statuses {
		byRevision[status.InstalledRevision] = status
	}
	if status := byRevision[installed]; status.State != "available" || status.CandidateRevision != candidate {
		t.Fatalf("available status = %+v", status)
	}
	if status := byRevision[candidate]; status.State != "current" || status.CandidateRevision != candidate {
		t.Fatalf("current status = %+v", status)
	}

	items, err := service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updates := map[string]*UpdateStatus{}
	for _, item := range items {
		updates[item.Name] = item.Update
	}
	if updates["alpha"] == nil || updates["alpha"].State != "available" {
		t.Fatalf("alpha cached update = %+v", updates["alpha"])
	}
	if updates["beta"] == nil || updates["beta"].State != "current" {
		t.Fatalf("beta cached update = %+v", updates["beta"])
	}
	if updates["local-only"] != nil {
		t.Fatalf("non-GitHub recipe update = %+v", updates["local-only"])
	}

	if _, err := service.checkUpdates(ctx, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 {
		t.Fatalf("fresh cached check called resolver %d times", resolveCalls)
	}
	repositories, err := queries.ListRecipeRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var repositoryID string
	for _, repository := range repositories {
		if repository.SourcePath == "alpha" {
			repositoryID = repository.ID
		}
	}
	if repositoryID == "" {
		t.Fatal("missing alpha repository")
	}
	if _, err := database.ExecContext(ctx, "UPDATE recipe_repositories SET tracking_ref = ? WHERE id = ?", "refs/tags/release", repositoryID); err != nil {
		t.Fatal(err)
	}
	service.resolveGitRef = func(_ context.Context, remote, trackingRef string) (string, error) {
		if remote != "https://github.com/MiaAI-Lab/shared" || trackingRef != "refs/tags/release" {
			t.Fatalf("tracked source = %q %q", remote, trackingRef)
		}
		return installed, nil
	}
	checked, err := service.CheckRepositoryUpdates(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if checked.State != "current" || checked.CandidateRevision != installed || checked.TrackingRef != "refs/tags/release" {
		t.Fatalf("tracking ref ignored: %+v", checked)
	}
	service.resolveGitRef = func(context.Context, string, string) (string, error) { return "", errors.New("fixture offline") }
	checked, err = service.CheckRepositoryUpdates(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if checked.State != "error" || checked.Error == "" || checked.CheckedAt == "" {
		t.Fatalf("failed check = %+v", checked)
	}
	// A fresh service reads the durable failure rather than reporting up to date.
	reopened, err := New(database, queries, events.NewEventBus(queries), validator, filepath.Join(root, "catalog"), filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	afterFailure, err := reopened.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Current == nil || afterFailure.Current.Update == nil ||
		afterFailure.Current.Update.State != "error" || afterFailure.HeadCheckError == "" || afterFailure.HeadCheckedAt != checked.CheckedAt ||
		afterFailure.UpdateAvailable {
		t.Fatalf("failure lost across reopen: %+v", afterFailure)
	}
}

func TestResolveGitRemoteRefUsesPinnedBranchAndPeeledTag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("commit", "--allow-empty", "--quiet", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("branch", "tracked")
	git("tag", "-a", "release", "-m", "release")
	git("commit", "--allow-empty", "--quiet", "-m", "later head")
	for _, ref := range []string{"tracked", "refs/heads/tracked", "release", "refs/tags/release"} {
		got, err := resolveGitRemoteRef(context.Background(), root, ref)
		if err != nil || got != base {
			t.Fatalf("ref %s resolved %s, %v; want %s", ref, got, err, base)
		}
	}
	git("branch", "release")
	if _, err := resolveGitRemoteRef(context.Background(), root, "release"); err == nil {
		t.Fatal("ambiguous branch/tag silently selected")
	}
}

func TestNormalizeGitHubRemoteRejectsUnsafeSources(t *testing.T) {
	valid, ok := normalizeGitHubRemote("https://github.com/MiaAI-Lab/example")
	if !ok || valid != "https://github.com/MiaAI-Lab/example" {
		t.Fatalf("valid remote = %q, %t", valid, ok)
	}
	for _, raw := range []string{
		"http://github.com/MiaAI-Lab/example",
		"https://example.com/MiaAI-Lab/example",
		"file:///etc/passwd",
		"https://github.com/MiaAI-Lab/example/extra",
		"https://user@github.com/MiaAI-Lab/example",
	} {
		if normalized, accepted := normalizeGitHubRemote(raw); accepted {
			t.Fatalf("unsafe remote %q normalized to %q", raw, normalized)
		}
	}
}

func createUpdateRecipe(t *testing.T, ctx context.Context, queries *db.Queries, name, revision, remote, sourcePath string) {
	t.Helper()
	manifest := &Manifest{
		APIVersion: APIVersion,
		Kind:       "Recipe",
		Metadata: Metadata{
			Name:        name,
			Version:     "1.0.0",
			Description: "update fixture",
			License:     "MIT",
			Source:      &Source{URL: remote, Revision: revision, Path: sourcePath},
		},
		Artifacts: []Artifact{},
		Workloads: []Workload{},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(name))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: name, Version: "1.0.0", Source: `{"type":"local","path":"fixture"}`,
		Manifest: string(manifestJSON),
	}); err != nil {
		t.Fatal(err)
	}
	row, err := queries.GetRecipe(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachRepositoryVersion(ctx, queries, manifest, digest, "", row.InstalledAt, "HEAD"); err != nil {
		t.Fatal(err)
	}
}
