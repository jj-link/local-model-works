package recipe_test

// TestGitInstallImmutability proves the pinned-Git install contract:
//
//  1. A recipe package committed at C1 installs from file://<repo>@C1
//     through the real import path; the stored digest equals the digest the
//     packer computes for the same directory.
//  2. Re-installing C1 is idempotent (same digest, one store entry).
//  3. After the source repo is deleted entirely, the installed digest still
//     resolves: manifest bytes come from the store, and the deploy launch
//     path (plan + create, what the scheduler calls) succeeds from the
//     digest alone.
//  4. Recreating the repo at the same path with a changed recipe (C2 ≠ C1)
//     does not disturb the installed package: re-resolving by digest D
//     yields byte-identical manifest and assets (no re-fetch, no drift);
//     C2 installs side-by-side as a second version.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// onlineNodes is a live node sender for the launch half of the proof.
type onlineNodes struct{}

func (onlineNodes) Send(string, *agentv1.ServerMessage) bool { return true }
func (onlineNodes) Online(string) bool                       { return true }

const immutableRecipeV1 = `apiVersion: localmodelworks/v1alpha1
kind: Recipe
metadata:
  name: lmw-immutable-demo
  version: 1.0.0
  displayName: Immutable demo
  description: Immutability proof package (v1).
  license: Apache-2.0
  source:
    url: https://fixtures.local/immutable-demo
    revision: "0000000000000000000000000000000000000000"
    path: .
compatibility:
  nodeCount: 1
artifacts: []
workloads:
  - image:
      reference: ghcr.io/localmodelworks/immutable-demo:1.0.0
      digest: sha256:2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a
    command:
      - /lmw/assets/serve.sh
    args:
      - --flag
      - one
    resources: {cpu: 1, memoryBytes: 16777216, pids: 64}
assets:
  - serve.sh
`

const immutableRecipeV2 = `apiVersion: localmodelworks/v1alpha1
kind: Recipe
metadata:
  name: lmw-immutable-demo
  version: 1.1.0
  displayName: Immutable demo
  description: Immutability proof package (v2, source changed).
  license: Apache-2.0
  source:
    url: https://fixtures.local/immutable-demo
    revision: "0000000000000000000000000000000000000000"
    path: .
compatibility:
  nodeCount: 1
artifacts: []
workloads:
  - image:
      reference: ghcr.io/localmodelworks/immutable-demo:1.1.0
      digest: sha256:3c4d5e6f708192a3b4c5d6e7f8091a4d5e6f708192a3b4c5d6e7f8091a2b3cd4
    command:
      - /lmw/assets/serve.sh
    args:
      - --flag
      - two
    resources: {cpu: 1, memoryBytes: 16777216, pids: 64}
assets:
  - serve.sh
`

const immutableAsset = "#!/bin/sh\necho immutable-asset\nexec sleep 3600\n"

func TestGitInstallImmutability(t *testing.T) {
	ctx := context.Background()
	v, err := recipe.NewValidator()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}

	// Fresh recipe store + deploy service over one migrated database.
	dbh, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw-immut.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	q := db.New(dbh)
	bus := events.NewEventBus(q)
	packageRoot := filepath.Join(t.TempDir(), "recipes")
	t.Cleanup(func() { _ = recipe.RemovePackage(packageRoot) })
	svc, err := recipe.New(dbh, q, bus, v, "", t.TempDir(), packageRoot)
	if err != nil {
		t.Fatalf("recipe service: %v", err)
	}
	caCA, err := ca.New()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	dep := deploy.New(dbh, q, bus, runs.New(dbh, q, bus, t.TempDir()), onlineNodes{}, caCA)

	repo := filepath.Join(t.TempDir(), "source")
	pkgDir := filepath.Join(repo, "pkg")
	writePackage := func(recipeYAML string) {
		t.Helper()
		if err := os.MkdirAll(pkgDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "recipe.yaml"), []byte(recipeYAML), 0o644); err != nil {
			t.Fatalf("write recipe: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "serve.sh"), []byte(immutableAsset), 0o755); err != nil {
			t.Fatalf("write asset: %v", err)
		}
	}
	commit := func(msg string) string {
		t.Helper()
		if _, err := gitRun(t, repo, "add", "-A"); err != nil {
			t.Fatalf("git add: %v", err)
		}
		if _, err := gitRun(t, repo, "commit", "-q", "-m", msg); err != nil {
			t.Fatalf("git commit: %v", err)
		}
		rev, err := gitOutput(t, repo, "rev-parse", "HEAD")
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		return rev
	}

	// 1. Build the repo, commit C1, and record what the packer computes for
	//    the same directory (the OCI layout `lmw recipe pack` would emit).
	writePackage(immutableRecipeV1)
	if _, err := gitRun(t, repo, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	c1 := commit("v1")
	if len(c1) != 40 {
		t.Fatalf("expected a 40-hex commit, got %q", c1)
	}
	_, res, err := recipe.PackFromDir(pkgDir, v)
	if err != nil {
		t.Fatalf("pack reference: %v", err)
	}
	layout := t.TempDir()
	if err := recipe.WriteLayout(layout, res); err != nil {
		t.Fatalf("write layout: %v", err)
	}
	ociManifestDigest, err := recipe.ReadLayoutDigest(layout)
	if err != nil {
		t.Fatalf("layout digest: %v", err)
	}

	// 2. Install from file://<repo>@C1 through the real import path.
	src := recipe.RecipeSource{Type: "git", Remote: "file://" + repo, Revision: c1, Path: "pkg"}
	rec, err := svc.Import(ctx, src)
	if err != nil {
		t.Fatalf("import @C1: %v", err)
	}
	d := rec.Digest
	if d != res.ManifestDigest {
		t.Fatalf("stored digest %q != packer manifest digest %q", d, res.ManifestDigest)
	}
	if rec.TrustState != recipe.TrustUntrusted {
		t.Fatalf("git import must be untrusted, got %q", rec.TrustState)
	}
	// Omitting the revision is the UI's normal path. The service resolves
	// default-branch HEAD once and stores the exact same immutable commit.
	autoPinned, err := svc.Import(ctx, recipe.RecipeSource{Type: "git", Remote: "file://" + repo, Path: "pkg"})
	if err != nil {
		t.Fatalf("import default HEAD: %v", err)
	}
	if autoPinned.Digest != rec.Digest {
		t.Fatalf("default HEAD digest %q != pinned digest %q", autoPinned.Digest, rec.Digest)
	}
	autoDetail, err := svc.Get(ctx, autoPinned.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var autoSource recipe.RecipeSource
	if err := json.Unmarshal(autoDetail.Source, &autoSource); err != nil {
		t.Fatal(err)
	}
	if autoSource.Revision != c1 {
		t.Fatalf("default HEAD stored revision %q, want %q", autoSource.Revision, c1)
	}
	// Snapshot the stored manifest bytes now; the immutability assertions
	// below compare every later resolution against these exact bytes.
	stored0, err := svc.Get(ctx, d)
	if err != nil {
		t.Fatalf("get after install: %v", err)
	}
	storedManifest := append([]byte(nil), stored0.Manifest...)
	var storedM recipe.Manifest
	if err := json.Unmarshal(storedManifest, &storedM); err != nil {
		t.Fatalf("stored manifest: %v", err)
	}
	if storedM.Metadata.Name != "lmw-immutable-demo" || storedM.Metadata.Version != "1.0.0" {
		t.Fatalf("stored manifest is not the installed recipe: %+v", storedM.Metadata)
	}

	// 3. Idempotent re-install: same digest, no duplicate store entry.
	recAgain, err := svc.Import(ctx, src)
	if err != nil {
		t.Fatalf("re-import @C1: %v", err)
	}
	if recAgain.Digest != d {
		t.Fatalf("re-import digest %q != %q", recAgain.Digest, d)
	}
	list, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly one store entry after idempotent re-install, got %d", len(list))
	}

	// 4. Delete the entire source repo; the installed digest must still
	//    resolve and the launch path must work from the digest alone.
	if err := os.RemoveAll(repo); err != nil {
		t.Fatalf("remove repo: %v", err)
	}
	detail, err := svc.Get(ctx, d)
	if err != nil {
		t.Fatalf("get installed digest without source: %v", err)
	}
	if !bytes.Equal(detail.Manifest, storedManifest) {
		t.Fatalf("installed manifest bytes drifted after source deletion")
	}
	// Approve, then drive the real launch path (plan + create) by digest:
	// no source fetch is possible — the repo no longer exists.
	if _, err := svc.SetTrust(ctx, d, recipe.TrustLocal, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := q.CreateNode(ctx, db.CreateNodeParams{ID: "node1", DisplayName: "node1", Labels: "{}"}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := q.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "online", ID: "node1"}); err != nil {
		t.Fatalf("node status: %v", err)
	}
	plan, err := dep.Plan(ctx, deploy.PlanRequest{RecipeDigest: d})
	if err != nil {
		t.Fatalf("plan from installed digest: %v", err)
	}
	if !plan.Ready || len(plan.Placements) != 1 || plan.Placements[0].NodeID != "node1" {
		t.Fatalf("plan from installed digest not ready: %+v", plan)
	}
	created, err := dep.Create(ctx, deploy.CreateRequest{RecipeDigest: d})
	if err != nil {
		t.Fatalf("create from installed digest: %v", err)
	}
	if _, err := dep.Get(ctx, created.ID); err != nil {
		t.Fatalf("get created deployment: %v", err)
	}

	// 5. Recreate the repo at the same path, change the recipe, commit C2.
	writePackage(immutableRecipeV2)
	if _, err := gitRun(t, repo, "init", "-q"); err != nil {
		t.Fatalf("git re-init: %v", err)
	}
	c2 := commit("v2 (changed)")
	if c2 == c1 {
		t.Fatalf("test setup: C2 must differ from C1")
	}
	rec2, err := svc.Import(ctx, recipe.RecipeSource{Type: "git", Remote: "file://" + repo, Revision: c2, Path: "pkg"})
	if err != nil {
		t.Fatalf("import @C2: %v", err)
	}
	if rec2.Digest == d {
		t.Fatalf("changed source must produce a different digest")
	}

	// 6. The installed C1 package is undisturbed: re-resolve by digest D.
	detail2, err := svc.Get(ctx, d)
	if err != nil {
		t.Fatalf("re-resolve installed digest: %v", err)
	}
	if !bytes.Equal(detail2.Manifest, storedManifest) {
		t.Fatalf("installed manifest bytes drifted after source change")
	}
	// Provenance still points at C1 — no re-fetch rewrote it.
	var srcRec struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(detail2.Source, &srcRec); err != nil {
		t.Fatalf("source: %v", err)
	}
	if srcRec.Revision != c1 {
		t.Fatalf("installed provenance revision %q != C1 %q", srcRec.Revision, c1)
	}
	// The on-disk OCI layout for the same package still verifies byte-for-byte.
	if err := recipe.VerifyLayout(layout); err != nil {
		t.Fatalf("layout verify: %v", err)
	}
	again, err := recipe.ReadLayoutDigest(layout)
	if err != nil {
		t.Fatalf("layout digest: %v", err)
	}
	if again != ociManifestDigest {
		t.Fatalf("layout digest changed: %q != %q", again, ociManifestDigest)
	}
	layerPath := filepath.Join(layout, "blobs", "sha256", res.LayerDigest[len("sha256:"):])
	layer, err := os.ReadFile(layerPath)
	if err != nil {
		t.Fatalf("read layer: %v", err)
	}
	dest := t.TempDir()
	if err := recipe.UnpackLayer(layer, dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	asset, err := os.ReadFile(filepath.Join(dest, "serve.sh"))
	if err != nil {
		t.Fatalf("read unpacked asset: %v", err)
	}
	if !bytes.Equal(asset, []byte(immutableAsset)) {
		t.Fatalf("unpacked asset bytes drifted")
	}
	// Side-by-side storage remains immutable, but inventory lists expose only
	// the most recently installed version for each recipe name.
	list2, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list2) != 1 {
		t.Fatalf("expected one current recipe entry, got %d: %v", len(list2), list2)
	}
	if list2[0].Digest != rec2.Digest {
		t.Fatalf("listed digest %q != newest installed digest %q", list2[0].Digest, rec2.Digest)
	}
	if list2[0].VersionCount != 2 {
		t.Fatalf("version count %d != 2", list2[0].VersionCount)
	}
}
