package recipe_test

// TestRecipeFixtures runs every case under internal/recipe/testdata through
// the real validator (plus the real pack/install/trust/plan paths where the
// case kind requires it) and asserts the exact stable code on failure.
// Each case dir holds recipe.yaml (the document), optional assets, and a
// case.json describing the expected outcome.
//
// Kinds:
//
//	validator — v.Validate must pass (outcome pass) or report the stable
//	            diagnostic code (outcome fail).
//	pack      — the document validates; PackFromDir must fail with the
//	            stable recipe.asset-* PackError code.
//	git       — Import{type:git} with a non-40-hex revision must fail with
//	            the stable sentinel before any clone.
//	gitpass   — Import{type:git, file://<repo>@<40-hex>} must succeed and
//	            store the recipe untrusted with the full commit recorded.
//	catalog   — the case dir is packed into an on-disk OCI layout under a
//	            temp catalog root; Import{type:catalog} must succeed.
//	trust     — the recipe is stored untrusted; the deploy Plan/Create
//	            trust gate must block it before any run/deployment row.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sigsign "github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// fixtureCase is one case.json.
type fixtureCase struct {
	Outcome  string   `json:"outcome"`
	Kind     string   `json:"kind,omitempty"` // validator (default) | pack | git | gitpass | trust | catalog
	Code     string   `json:"code,omitempty"`
	Revision string   `json:"revision,omitempty"`
	HighRisk []string `json:"highRisk,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// offlineNodes answers the deploy service without a live fleet.
type offlineNodes struct{}

func (offlineNodes) Send(string, *agentv1.ServerMessage) bool { return false }
func (offlineNodes) Online(string) bool                       { return false }

// fixtureEnv wires the real validator, recipe store, and deploy service
// against a fresh migrated database.
type fixtureEnv struct {
	ctx           context.Context
	t             *testing.T
	validator     *recipe.Validator
	dbh           *sql.DB
	q             *db.Queries
	svc           *recipe.Service
	dep           *deploy.Service
	catalogRoot   string
	catalogSigner *sigsign.EphemeralKeypair
	gitRepo       string // temp repo holding testdata/pass-git-pinned under "pkg"
}

func newFixtureEnv(t *testing.T) *fixtureEnv {
	t.Helper()
	ctx := context.Background()
	dbh, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw-test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	q := db.New(dbh)
	bus := events.NewEventBus(q)
	v, err := recipe.NewValidator()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	catalogRoot := t.TempDir()
	signer, err := sigsign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatalf("catalog signer: %v", err)
	}
	publicKey, err := signer.GetPublicKeyPem()
	if err != nil {
		t.Fatalf("catalog public key: %v", err)
	}
	trustPath := filepath.Join(t.TempDir(), "trust.pem")
	if err := os.WriteFile(trustPath, []byte(publicKey), 0o600); err != nil {
		t.Fatalf("catalog trust key: %v", err)
	}
	packageRoot := filepath.Join(t.TempDir(), "recipes")
	t.Cleanup(func() { _ = recipe.RemovePackage(packageRoot) })
	svc, err := recipe.New(dbh, q, bus, v, trustPath, catalogRoot, packageRoot)
	if err != nil {
		t.Fatalf("recipe service: %v", err)
	}
	caCA, err := ca.New()
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	dep := deploy.New(dbh, q, bus, runs.New(dbh, q, bus, t.TempDir()), offlineNodes{}, caCA)

	// One committed temp repo (the pass-git-pinned package under "pkg")
	// serves both git fixture kinds.
	repo := t.TempDir()
	pkgDir := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src, err := filepath.Abs(filepath.Join("testdata", "pass-git-pinned"))
	if err != nil {
		t.Fatalf("testdata: %v", err)
	}
	for _, f := range []string{"recipe.yaml", "serve.sh"} {
		data, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, f), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if _, err := gitRun(t, repo, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := gitRun(t, repo, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := gitRun(t, repo, "commit", "-q", "-m", "fixture"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	rev, err := gitOutput(t, repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	_ = rev
	return &fixtureEnv{
		ctx: ctx, t: t, validator: v, dbh: dbh, q: q, svc: svc, dep: dep,
		catalogRoot: catalogRoot, catalogSigner: signer, gitRepo: repo,
	}
}

func (e *fixtureEnv) gitHead(t *testing.T) string {
	t.Helper()
	rev, err := gitOutput(t, e.gitRepo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return rev
}

// gitRun runs git inside dir with a deterministic identity.
func gitRun(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=LMW Fixture", "GIT_AUTHOR_EMAIL=fixture@localmodelworks.dev",
		"GIT_COMMITTER_NAME=LMW Fixture", "GIT_COMMITTER_EMAIL=fixture@localmodelworks.dev",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

func gitOutput(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := gitRun(t, dir, args...)
	return strings.TrimSpace(out), err
}

// loadDoc converts a case dir's recipe document to raw JSON.
func loadDoc(t *testing.T, dir string) []byte {
	t.Helper()
	for _, name := range []string{"recipe.yaml", "recipe.json"} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err == nil {
			doc, err := recipe.YAMLOrJSON(data)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return doc
		}
	}
	t.Fatalf("no recipe document in %s", dir)
	return nil
}

func errorCodes(diags []recipe.Diagnostic) map[string]bool {
	out := map[string]bool{}
	for _, d := range diags {
		if d.Severity == "error" {
			out[d.Code] = true
		}
	}
	return out
}

func TestRecipeFixtures(t *testing.T) {
	env := newFixtureEnv(t)
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("testdata: %v", err)
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "outside" {
			continue
		}
		dir := filepath.Join("testdata", e.Name())
		caseJSON, err := os.ReadFile(filepath.Join(dir, "case.json"))
		if err != nil {
			t.Fatalf("%s: missing case.json: %v", e.Name(), err)
		}
		var fc fixtureCase
		if err := json.Unmarshal(caseJSON, &fc); err != nil {
			t.Fatalf("%s: case.json: %v", e.Name(), err)
		}
		if fc.Kind == "" {
			fc.Kind = "validator"
		}
		name, fc := e.Name(), fc
		count++
		t.Run(name, func(t *testing.T) {
			runFixture(t, env, fc, dir)
		})
	}
	if count < 12 {
		t.Fatalf("only %d fixture cases found; the suite expects >= 12", count)
	}
}

func runFixture(t *testing.T, env *fixtureEnv, fc fixtureCase, dir string) {
	t.Helper()
	switch fc.Kind {
	case "validator":
		doc := loadDoc(t, dir)
		diags, err := env.validator.Validate(doc)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		codes := errorCodes(diags)
		if fc.Outcome == "pass" {
			if len(codes) > 0 {
				t.Fatalf("expected pass, got error diagnostics: %+v", diags)
			}
			m, err := recipe.Parse(doc)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			high := map[string]bool{}
			for _, p := range m.HighRiskPermissions() {
				high[p] = true
			}
			for _, want := range fc.HighRisk {
				if !high[want] {
					t.Fatalf("pass case must surface high-risk permission %q; got %v", want, m.HighRiskPermissions())
				}
			}
			return
		}
		if !codes[fc.Code] {
			t.Fatalf("expected stable code %q, got diagnostics: %+v", fc.Code, diags)
		}
	case "pack":
		doc := loadDoc(t, dir)
		diags, err := env.validator.Validate(doc)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if c := errorCodes(diags); len(c) > 0 {
			t.Fatalf("pack case must pass validation; got: %+v", diags)
		}
		_, _, err = recipe.PackFromDir(dir, env.validator)
		if err == nil {
			t.Fatalf("PackFromDir must fail with a stable asset code")
		}
		var pe *recipe.PackError
		if !errors.As(err, &pe) {
			t.Fatalf("expected *recipe.PackError, got %T: %v", err, err)
		}
		if pe.Code != fc.Code {
			t.Fatalf("expected stable code %q, got %q (%s)", fc.Code, pe.Code, pe.Message)
		}
	case "git":
		// A short ref must fail before any clone, with the stable sentinel.
		_, err := env.svc.Import(env.ctx, recipe.RecipeSource{
			Type: "git", Remote: "file://" + env.gitRepo,
			Revision: fc.Revision, Path: "pkg",
		})
		if !errors.Is(err, recipe.ErrUnpinnedRevision) {
			t.Fatalf("expected ErrUnpinnedRevision, got %v", err)
		}
	case "gitpass":
		rev := env.gitHead(t)
		rec, err := env.svc.Import(env.ctx, recipe.RecipeSource{
			Type: "git", Remote: "file://" + env.gitRepo,
			Revision: rev, Path: "pkg",
		})
		if err != nil {
			t.Fatalf("git import: %v", err)
		}
		if rec.TrustState != recipe.TrustUntrusted {
			t.Fatalf("git import must store untrusted, got %q", rec.TrustState)
		}
		var src struct {
			Revision string `json:"revision"`
			Tree     string `json:"tree"`
		}
		if err := json.Unmarshal(rec.Source, &src); err != nil {
			t.Fatalf("source: %v", err)
		}
		tree, err := gitOutput(t, env.gitRepo, "rev-parse", "HEAD^{tree}")
		if err != nil {
			t.Fatalf("tree: %v", err)
		}
		if src.Revision != rev || src.Tree != tree {
			t.Fatalf("import provenance: revision=%q tree=%q, want revision=%q tree=%q", src.Revision, src.Tree, rev, tree)
		}
	case "catalog":
		m, res, err := recipe.PackFromDir(dir, env.validator)
		if err != nil {
			t.Fatalf("pack: %v", err)
		}
		layoutDir := filepath.Join(env.catalogRoot, "packages", m.Metadata.Name)
		if err := recipe.WriteLayout(layoutDir, res); err != nil {
			t.Fatalf("layout: %v", err)
		}
		index := map[string]any{
			"apiVersion": "localmodelworks/v1alpha1",
			"kind":       "RecipeCatalog",
			"metadata":   map[string]any{"name": "test-catalog"},
			"recipes": []any{map[string]any{
				"name": m.Metadata.Name, "version": m.Metadata.Version, "summary": "fixture",
				"oci": map[string]any{"reference": "file://" + layoutDir, "digest": res.ManifestDigest},
			}},
		}
		indexJSON, err := json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := recipe.Canonical(indexJSON)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := sigsign.Bundle(&sigsign.PlainData{Data: canonical}, env.catalogSigner, sigsign.BundleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		bundleJSON, err := protojson.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(env.catalogRoot, "catalog.json"), indexJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(env.catalogRoot, "catalog.sigstore.json"), bundleJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		rec, err := env.svc.Import(env.ctx, recipe.RecipeSource{Type: "catalog", Reference: m.Metadata.Name})
		if err != nil {
			t.Fatalf("catalog import: %v", err)
		}
		if rec.Digest != res.ManifestDigest {
			t.Fatalf("digest %q != packed manifest digest %q", rec.Digest, res.ManifestDigest)
		}
		if rec.TrustState != recipe.TrustVerified {
			t.Fatalf("signed catalog import must be verified, got %q", rec.TrustState)
		}
		for _, artifact := range m.Artifacts {
			identity, err := artifactidentity.Canonical(
				artifact.Source.Type, artifact.Source.Identity, artifact.Source.Revision, artifact.Source.Digest,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.q.GetArtifactByIdentity(env.ctx, identity); err != nil {
				t.Fatalf("installed recipe did not upsert artifact %s: %v", identity, err)
			}
		}
	case "trust":
		doc := loadDoc(t, dir)
		rec, err := env.svc.Store(env.ctx, doc, recipe.RecipeSource{Type: "local", Path: dir}, recipe.TrustUntrusted)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		if _, err := env.dep.Plan(env.ctx, deploy.PlanRequest{RecipeDigest: rec.Digest}); !errors.Is(err, deploy.ErrUntrusted) {
			t.Fatalf("Plan must block with ErrUntrusted, got %v", err)
		}
		if _, err := env.dep.Create(env.ctx, deploy.CreateRequest{RecipeDigest: rec.Digest}); !errors.Is(err, deploy.ErrUntrusted) {
			t.Fatalf("Create must block with ErrUntrusted, got %v", err)
		}
		deps, err := env.q.ListDeployments(env.ctx)
		if err != nil {
			t.Fatalf("list deployments: %v", err)
		}
		runRows, err := env.q.ListRuns(env.ctx, db.ListRunsParams{})
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		if len(deps) != 0 || len(runRows) != 0 {
			t.Fatalf("untrusted launch must create nothing: %d deployments, %d runs", len(deps), len(runRows))
		}
		// After operator approval the gate opens: planning proceeds to the
		// (empty-fleet) capacity check instead of the trust error.
		if _, err := env.svc.SetTrust(env.ctx, rec.Digest, recipe.TrustLocal, true); err != nil {
			t.Fatalf("set trust: %v", err)
		}
		plan, err := env.dep.Plan(env.ctx, deploy.PlanRequest{RecipeDigest: rec.Digest})
		if err != nil {
			t.Fatalf("Plan after approval must pass the trust gate, got %v", err)
		}
		if plan == nil || plan.Ready {
			t.Fatalf("empty-fleet plan must be not-ready, got %+v", plan)
		}

	default:
		t.Fatalf("unknown fixture kind %q", fc.Kind)
	}
}
func TestCatalogRejectsTamperedSignedIndex(t *testing.T) {
	env := newFixtureEnv(t)
	index := map[string]any{
		"apiVersion": "localmodelworks/v1alpha1",
		"kind":       "RecipeCatalog",
		"metadata":   map[string]any{"name": "signed-catalog"},
		"recipes": []any{map[string]any{
			"name": "catalog-demo", "version": "1.0.0", "summary": "signed",
			"oci": map[string]any{
				"reference": "registry.example.test/lmw/catalog-demo",
				"digest":    "sha256:" + strings.Repeat("a", 64),
			},
		}},
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := recipe.Canonical(indexJSON)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := sigsign.Bundle(&sigsign.PlainData{Data: canonical}, env.catalogSigner, sigsign.BundleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.catalogRoot, "catalog.sigstore.json"), bundleJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	index["metadata"] = map[string]any{"name": "tampered-catalog"}
	tampered, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(env.catalogRoot, "catalog.json"), tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Import(env.ctx, recipe.RecipeSource{Type: "catalog", Reference: "catalog-demo"}); err == nil ||
		!strings.Contains(err.Error(), "catalog signature") {
		t.Fatalf("tampered catalog error = %v", err)
	}
}
