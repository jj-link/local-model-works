package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
)

const (
	fixtureRevA = "1234567890abcdef1234567890abcdef12345678"
	fixtureRevD = "fedcba0987654321fedcba0987654321fedcba09"
)

var (
	fixtureImageSHA = "sha256:" + strings.Repeat("aa", 32)
	fixtureImageRef = "ghcr.io/fixture/vllm-fixture@" + fixtureImageSHA
)

const (
	runSucceeded = "11111111-1111-4111-8111-111111111111"
	runFailed    = "22222222-2222-4222-8222-222222222222"
	runCancelled = "33333333-3333-4333-8333-333333333333"
	runRunning   = "44444444-4444-4444-8444-444444444444"
	ghostRun     = "55555555-5555-4555-8555-555555555555"
)

func legacyDirs() (string, string) {
	return filepath.Join("testdata", "legacy-fixture"), filepath.Join("testdata", "state-fixture")
}

// testScanOptions isolates the scan from the developer machine: the legacy
// default cache roots ($HOME/.cache/huggingface, $HOME/models) are moved to
// an empty temp dir, and docker ps is pointed at a nonexistent socket so
// the container report is deterministically empty.
func testScanOptions(t *testing.T) ScanOptions {
	t.Helper()
	legacy, state := legacyDirs()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/lmw-migrate-test.sock")
	return ScanOptions{LegacyDir: legacy, StateDir: state, LegacyRevision: fixtureRevA}
}

func scanFixture(t *testing.T) *Report {
	t.Helper()
	rep, err := Scan(testScanOptions(t))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return rep
}

func writePlan(t *testing.T, rep *Report) (string, *Plan) {
	t.Helper()
	b, err := PlanJSON(&rep.Plan)
	if err != nil {
		t.Fatalf("plan json: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("round-trip plan: %v", err)
	}
	return path, &p
}

// (1) Determinism: two scans over the same inputs produce the identical
// digest and byte-identical plan documents.
func TestScanDeterministic(t *testing.T) {
	opts := testScanOptions(t)
	rep1, err := Scan(opts)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	rep2, err := Scan(opts)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}

	if rep1.Digest == "" || !strings.HasPrefix(rep1.Digest, "sha256:") ||
		len(rep1.Digest) != len("sha256:")+64 {
		t.Fatalf("digest shape: %q", rep1.Digest)
	}
	if rep1.Digest != rep2.Digest {
		t.Fatalf("digests differ:\nfirst  %s\nsecond %s", rep1.Digest, rep2.Digest)
	}
	b1, _ := PlanJSON(&rep1.Plan)
	b2, _ := PlanJSON(&rep2.Plan)
	if !bytes.Equal(b1, b2) {
		t.Fatal("plan JSON differs between identical scans")
	}
	if rep1.Plan.Digest != rep1.Digest {
		t.Fatalf("plan digest field %s != report digest %s", rep1.Plan.Digest, rep1.Digest)
	}
}

// (2) Report correctness: counts, strays, mutable images, nonterminal ids,
// run-to-recipe mapping, cluster draft, container report.
func TestScanReport(t *testing.T) {
	rep := scanFixture(t)
	p := &rep.Plan

	c := p.Counts
	if c.SingleNodePackages != 5 {
		t.Errorf("single-node packages: got %d want 5", c.SingleNodePackages)
	}
	if c.SingleNodeRecipes != 4 {
		t.Errorf("single-node recipes: got %d want 4 (alpha merged)", c.SingleNodeRecipes)
	}
	if c.MergedRecipes != 1 {
		t.Errorf("merged recipes: got %d want 1", c.MergedRecipes)
	}
	if c.ClusterPackages != 1 {
		t.Errorf("cluster packages: got %d want 1", c.ClusterPackages)
	}
	if c.Strays != 1 {
		t.Errorf("strays: got %d want 1", c.Strays)
	}
	if c.MutableImages != 1 {
		t.Errorf("mutable images: got %d want 1", c.MutableImages)
	}
	if c.RunsTerminal != 3 || c.RunsNonterminal != 1 {
		t.Errorf("runs: terminal %d nonterminal %d (want 3/1)", c.RunsTerminal, c.RunsNonterminal)
	}
	if c.BenchmarkIndexEntries != 3 || c.BenchmarkResultsFiles != 2 || c.AiderBenchmarkFiles != 1 {
		t.Errorf("benchmarks: index %d results %d aider %d (want 3/2/1)",
			c.BenchmarkIndexEntries, c.BenchmarkResultsFiles, c.AiderBenchmarkFiles)
	}
	if c.Placements != 2 || c.PlacementFailures != 0 {
		t.Errorf("placements: %d failures %d (want 2/0)", c.Placements, c.PlacementFailures)
	}

	if len(p.Strays) != 1 || !strings.Contains(p.Strays[0].Path, "llama.cpp") {
		t.Errorf("stray paths: %+v (want the llama.cpp tree)", p.Strays)
	}

	if got := NonterminalIDs(p.Runs); len(got) != 1 || got[0] != runRunning {
		t.Errorf("nonterminal ids: %v (want [%s])", got, runRunning)
	}

	byID := map[string]RunEntry{}
	for _, r := range p.Runs {
		byID[r.ID] = r
	}
	r1 := byID[runSucceeded]
	if r1.RecipeDigest == "" {
		t.Error("succeeded serving run did not map to a converted recipe digest")
	}
	if r1.LegacyIdentity != "" {
		t.Errorf("mapped run also has legacy identity %q", r1.LegacyIdentity)
	}
	r2 := byID[runFailed]
	if r2.LegacyIdentity != runFailed {
		t.Errorf("failed benchmark run legacy identity: %q (want the run id)", r2.LegacyIdentity)
	}
	if r2.State != "failed" || r2.LegacyState != "cleanup_failed" {
		t.Errorf("failed run mapping: state %q legacy %q (want failed/cleanup_failed)",
			r2.State, r2.LegacyState)
	}
	r3 := byID[runCancelled]
	if r3.State != "cancelled" {
		t.Errorf("cancelled run state: %q", r3.State)
	}
	// beta_mut's request targets spark1, but the beta recipe only covers
	// local: it must not map to a digest.
	if r3.RecipeDigest != "" {
		t.Errorf("cancelled run (artifact on a non-covering target) mapped to %s", r3.RecipeDigest)
	}

	var mutable *RecipeEntry
	for i := range p.Recipes {
		if p.Recipes[i].Image.Mutable {
			mutable = &p.Recipes[i]
		}
	}
	if mutable == nil || mutable.Image.Reference != "fixture-local/beta-vllm:dev1" {
		t.Errorf("mutable recipe: %+v (want beta-mut)", mutable)
	}

	if len(p.ClusterDrafts) != 1 {
		t.Fatalf("cluster drafts: %d (want 1)", len(p.ClusterDrafts))
	}
	d := p.ClusterDrafts[0]
	if d.HeadHost != "spark2-ts" || d.WorkerHost != "spark3-ts" || d.APIPort != 8888 {
		t.Errorf("cluster draft hosts: %s/%s port %d", d.HeadHost, d.WorkerHost, d.APIPort)
	}
	if len(d.Profiles) != 2 || d.DefaultProfile != "quality" {
		t.Errorf("cluster profiles: %d default %q (want 2/quality)", len(d.Profiles), d.DefaultProfile)
	}
	if len(d.Ranks) != 2 {
		t.Errorf("cluster ranks: %d (want 2)", len(d.Ranks))
	}

	if rep.Containers != nil {
		t.Errorf("containers with unreachable docker: %+v (want nil)", rep.Containers)
	}
}

func findRecipe(t *testing.T, p *Plan, name string) *RecipeEntry {
	t.Helper()
	for i := range p.Recipes {
		if p.Recipes[i].Name == name {
			return &p.Recipes[i]
		}
	}
	t.Fatalf("recipe %s not in plan (have %v)", name, recipeNames(p))
	return nil
}

func recipeNames(p *Plan) []string {
	out := make([]string, 0, len(p.Recipes))
	for _, r := range p.Recipes {
		out = append(out, r.Name)
	}
	return out
}

// (3) Recipe conversion: every converted document validates against the
// v1alpha1 schema and semantic rules (mutable images only carry the
// image-latest finding), exact digests/revisions are carried, and the
// dedup rule merges identical target copies while splitting divergent
// revisions.
func TestRecipeConversion(t *testing.T) {
	rep := scanFixture(t)
	p := &rep.Plan

	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	// The legacy parser itself rejects :latest, so every migrated document
	// must validate clean; mutable (versioned local tag) images are
	// flagged in the plan, not by a validator diagnostic.
	for _, r := range p.Recipes {
		diags, err := validator.Validate(r.Document)
		if err != nil {
			t.Errorf("recipe %s: validate: %v", r.Name, err)
			continue
		}
		if len(diags) > 0 {
			t.Errorf("recipe %s: diagnostics %+v (want none)", r.Name, diags)
		}
	}

	alpha := findRecipe(t, p, "alpha-fp8")
	if !alpha.Merged {
		t.Fatal("alpha-fp8 should be a merged capability-selected recipe")
	}
	if len(alpha.Packages) != 2 {
		t.Fatalf("alpha-fp8 packages: %v (want the rtx6000 + spark copies)", alpha.Packages)
	}
	if got := strings.Join(alpha.Targets, ","); got != "local,spark1,spark2,spark3" {
		t.Errorf("alpha-fp8 targets: %s", got)
	}
	if got := strings.Join(alpha.Architectures, ","); got != "sm_120,sm_121" {
		t.Errorf("alpha-fp8 architectures: %s", got)
	}

	var doc map[string]any
	if err := json.Unmarshal(alpha.Document, &doc); err != nil {
		t.Fatalf("alpha document: %v", err)
	}
	workloads, _ := doc["workloads"].([]any)
	if len(workloads) != 2 {
		t.Fatalf("alpha workloads: %d (want one per capability variant)", len(workloads))
	}
	w0 := workloads[0].(map[string]any)
	img := w0["image"].(map[string]any)
	if img["reference"] != fixtureImageRef || img["digest"] != fixtureImageSHA {
		t.Errorf("alpha image: ref %v digest %v", img["reference"], img["digest"])
	}
	artifacts := doc["artifacts"].([]any)
	byName := map[string]map[string]any{}
	for _, a := range artifacts {
		am := a.(map[string]any)
		byName[am["name"].(string)] = am
	}
	model := byName["model"]
	src := model["source"].(map[string]any)
	if src["type"] != "huggingface" || src["identity"] != "hf://fixture-org/alpha-model" ||
		src["revision"] != fixtureRevA {
		t.Errorf("alpha model artifact: %+v", src)
	}
	drafter := byName["drafter"]
	if drafter == nil || drafter["source"].(map[string]any)["revision"] != fixtureRevD {
		t.Errorf("alpha drafter artifact: %+v", drafter)
	}
	env := w0["env"].(map[string]any)
	wantModelPath := "/models/hub/models--fixture-org--alpha-model/snapshots/" + fixtureRevA
	if env["MODEL_PATH"] != wantModelPath {
		t.Errorf("alpha MODEL_PATH: %v (want %s)", env["MODEL_PATH"], wantModelPath)
	}

	beta := findRecipe(t, p, "beta-mut")
	if !beta.Image.Mutable {
		t.Error("beta-mut should carry a mutable image")
	}
	if len(beta.Targets) != 1 || beta.Targets[0] != "local" {
		t.Errorf("beta-mut targets: %v", beta.Targets)
	}

	gamma := findRecipe(t, p, "gamma-host")
	if gamma.Merged {
		t.Error("gamma-host should be a single (non-merged) recipe")
	}
	gdoc := map[string]any{}
	if err := json.Unmarshal(gamma.Document, &gdoc); err != nil {
		t.Fatalf("gamma document: %v", err)
	}
	gart := map[string]map[string]any{}
	for _, a := range gdoc["artifacts"].([]any) {
		am := a.(map[string]any)
		gart[am["name"].(string)] = am
	}
	if ts := gart["model"]["source"].(map[string]any)["type"]; ts != "file" {
		t.Errorf("gamma model source type: %v (want file for a host-mounted model)", ts)
	}

	delta := findRecipe(t, p, "delta-fp8")
	// the spark serve profile covers all three spark nodes.
	if got := strings.Join(delta.Targets, ","); got != "spark1,spark2,spark3" {
		t.Errorf("delta-fp8 targets: %s (want spark1,spark2,spark3)", got)
	}
	if delta.ModelRevision == "" {
		t.Error("delta-fp8 lost its model revision")
	}
}

// (4) Import into a fresh state root: recipes land as trust_local, run
// states are mapped with legacy markers, logs are readable through the
// runs service with the recorded end offset, benchmark counts are
// reproduced, placements are registered, and every source file is
// untouched.
func TestImport(t *testing.T) {
	ctx := context.Background()
	opts := testScanOptions(t)
	rep := scanFixture(t)
	planFile, _ := writePlan(t, rep)
	target := t.TempDir()

	imp, err := Import(ctx, ImportOptions{Scan: opts, PlanFile: planFile, TargetRoot: target, Force: true})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !imp.SourceUntouched {
		t.Errorf("source trees changed: before %s after %s", imp.SourceDigestBefore, imp.SourceDigestAfter)
	}
	if imp.PlanDigest != rep.Digest || imp.RescanDigest != rep.Digest {
		t.Errorf("digests: plan %s rescan %s want %s", imp.PlanDigest, imp.RescanDigest, rep.Digest)
	}
	if imp.RecipesImported != 4 {
		t.Errorf("recipes imported: %d (want 4)", imp.RecipesImported)
	}
	if imp.RunsImported != 3 {
		t.Errorf("runs imported: %d (want 3; the nonterminal run is skipped with --force)", imp.RunsImported)
	}
	if imp.GhostRunsCreated != 1 {
		t.Errorf("ghost runs: %d (want 1 for the cross-agent result's foreign run)", imp.GhostRunsCreated)
	}
	if imp.LogsImported != 3 {
		t.Errorf("logs imported: %d (want 3)", imp.LogsImported)
	}
	if imp.BenchmarkRows != 2 {
		t.Errorf("benchmark rows: %d (want 2: file A run r2, file B run r5)", imp.BenchmarkRows)
	}
	if imp.BenchmarkFilesLinked+imp.BenchmarkFilesCopied != 3 {
		t.Errorf("benchmark files: linked %d copied %d (want 3 total: 2 results + 1 aider)",
			imp.BenchmarkFilesLinked, imp.BenchmarkFilesCopied)
	}
	if imp.PlacementsRegistered != 2 || len(imp.PlacementFailures) != 0 {
		t.Errorf("placements: registered %d failures %v (want 2/none)",
			imp.PlacementsRegistered, imp.PlacementFailures)
	}
	if !strings.Contains(strings.Join(imp.Warnings, "\n"), "beta-mut") {
		t.Errorf("mutable recipe warning missing: %v", imp.Warnings)
	}

	// Verify through the real services, not the report.
	sqlDB, err := db.Open(ctx, filepath.Join(target, "lmw.db"))
	if err != nil {
		t.Fatalf("open imported db: %v", err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)

	if row, err := q.GetRun(ctx, runSucceeded); err != nil || row.State != "succeeded" || row.Module != LegacyModule {
		t.Errorf("run %s: %+v err %v (want succeeded/legacy)", runSucceeded, row, err)
	}
	failed, err := q.GetRun(ctx, runFailed)
	if err != nil {
		t.Fatalf("run %s: %v", runFailed, err)
	}
	if failed.State != "failed" || !failed.LegacyIdentity.Valid || failed.LegacyIdentity.String != runFailed {
		t.Errorf("run %s: state %q identity %+v (want failed + legacy marker)",
			runFailed, failed.State, failed.LegacyIdentity)
	}
	if !failed.ErrorMessage.Valid || !strings.Contains(failed.ErrorMessage.String, "cleanup_failed") {
		t.Errorf("run %s error message: %+v (want the original legacy state)", runFailed, failed.ErrorMessage)
	}
	if row, err := q.GetRun(ctx, runCancelled); err != nil || row.State != "cancelled" {
		t.Errorf("run %s: %+v err %v (want cancelled)", runCancelled, row, err)
	}
	if _, err := q.GetRun(ctx, runRunning); err == nil {
		t.Error("nonterminal run was imported despite --force skipping it")
	}
	if ghost, err := q.GetRun(ctx, ghostRun); err != nil || ghost.State != "succeeded" {
		t.Errorf("ghost run: %+v err %v (want succeeded)", ghost, err)
	}
	for _, r := range rep.Plan.Recipes {
		if rec, err := q.GetRecipe(ctx, r.Digest); err != nil || rec.TrustState != string(recipe.TrustLocal) {
			t.Errorf("recipe %s: trust %q err %v (want local)", r.Name, rec.TrustState, err)
		}
	}

	bus := events.NewEventBus(q)
	runsSvc := runs.New(sqlDB, q, bus, filepath.Join(target, "runs"))
	got, err := runsSvc.Get(ctx, runFailed)
	if err != nil {
		t.Fatalf("runs service get: %v", err)
	}
	if got.State != "failed" || got.LegacyIdentity == nil || *got.LegacyIdentity != runFailed {
		t.Errorf("runs service view: state %q identity %v", got.State, got.LegacyIdentity)
	}
	_, stateDir := legacyDirs()
	logSrc, _ := os.ReadFile(filepath.Join(stateDir, "runs", runSucceeded, "output.log"))
	// The log file lives at the runs service's path with the source size;
	// the terminal end offset itself is the in-memory MarkLogEnd marker the
	// import recorded for this run's stream.
	logPath := runsSvc.LogPath(runSucceeded, "", 0, "stdout")
	if logPath == "" {
		t.Fatal("log path: unsafe id")
	}
	if st, err := os.Stat(logPath); err != nil || st.Size() != int64(len(logSrc)) {
		t.Errorf("imported log: size %v err %v (want %d)", st, err, len(logSrc))
	}
	chunk, _, size, err := runsSvc.ReadLog(runSucceeded, "", 0, "stdout", 0, 1<<20)
	if err != nil || size != uint64(len(logSrc)) || !bytes.Equal(chunk, logSrc) {
		t.Errorf("log read: size %d err %v (want %d bytes of the source log)", size, err, len(logSrc))
	}

	// Benchmark files must exist in the new store with identical content.
	for sub, dst := range map[string]string{
		"benchmark-results": "results", "aider-benchmarks": "aider-benchmarks",
	} {
		srcRoot := filepath.Join(stateDir, sub)
		dstRoot := filepath.Join(target, "benchmarks", dst)
		seen := 0
		filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			seen++
			rel, _ := filepath.Rel(srcRoot, path)
			sb, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read source file %s: %v", rel, err)
			}
			dbb, err := os.ReadFile(filepath.Join(dstRoot, rel))
			if err != nil {
				t.Fatalf("imported file missing: %s: %v", rel, err)
			}
			if !bytes.Equal(sb, dbb) {
				t.Errorf("imported file %s differs from source", rel)
			}
			return nil
		})
		if seen == 0 {
			t.Fatalf("source %s is empty; fixture broken", sub)
		}
	}
}

func TestImportMapsLegacyNodesToExistingEnrollment(t *testing.T) {
	ctx := context.Background()
	opts := testScanOptions(t)
	rep := scanFixture(t)
	planFile, _ := writePlan(t, rep)
	target := t.TempDir()
	nodeMap := map[string]string{
		"local":  "agent-local",
		"spark1": "agent-spark1",
		"spark2": "agent-spark2",
		"spark3": "agent-spark3",
	}

	sqlDB, err := db.Open(ctx, filepath.Join(target, "lmw.db"))
	if err != nil {
		t.Fatalf("open target db: %v", err)
	}
	q := db.New(sqlDB)
	for legacyID, nodeID := range nodeMap {
		if err := q.CreateNode(ctx, db.CreateNodeParams{
			ID:          nodeID,
			DisplayName: legacyID,
			Labels:      "{}",
			CreatedAt:   nowStamp(),
		}); err != nil {
			t.Fatalf("seed enrolled node %s: %v", legacyID, err)
		}
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close seeded target db: %v", err)
	}

	imp, err := Import(ctx, ImportOptions{
		Scan:       opts,
		PlanFile:   planFile,
		TargetRoot: target,
		Force:      true,
		NodeMap:    nodeMap,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	sqlDB, err = db.Open(ctx, filepath.Join(target, "lmw.db"))
	if err != nil {
		t.Fatalf("reopen imported db: %v", err)
	}
	defer sqlDB.Close()
	q = db.New(sqlDB)
	placements := 0
	for legacyID, nodeID := range nodeMap {
		if _, err := q.GetNode(ctx, nodeID); err != nil {
			t.Errorf("mapped node %s (%s): %v", legacyID, nodeID, err)
		}
		if _, err := q.GetNode(ctx, legacyID); err == nil {
			t.Errorf("legacy placeholder node %s was created despite node mapping", legacyID)
		}
		rows, err := q.ListPlacementsOnNode(ctx, nodeID)
		if err != nil {
			t.Fatalf("placements on mapped node %s: %v", nodeID, err)
		}
		placements += len(rows)
	}
	if placements != imp.PlacementsRegistered {
		t.Errorf("mapped placements: %d (want %d)", placements, imp.PlacementsRegistered)
	}
}

// (5a) Import digest mismatch: a plan whose content no longer matches its
// recorded digest fails with both digests named; --force overrides.
func TestImportDigestMismatch(t *testing.T) {
	ctx := context.Background()
	opts := testScanOptions(t)
	rep := scanFixture(t)
	planFile, p := writePlan(t, rep)
	p.Strays[0].Reason = "tampered"
	p.Digest = PlanDigestOf(p)
	tb, err := PlanJSON(p)
	if err != nil {
		t.Fatalf("plan json: %v", err)
	}
	if err := os.WriteFile(planFile, tb, 0o644); err != nil {
		t.Fatalf("write tampered plan: %v", err)
	}

	imp, err := Import(ctx, ImportOptions{Scan: opts, PlanFile: planFile, TargetRoot: t.TempDir(), Force: false})
	if err == nil {
		t.Fatal("import with a tampered plan digest should fail")
	}
	for _, want := range []string{"plan digest mismatch", imp.PlanDigest, imp.RescanDigest} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error %q should name %q", err.Error(), want)
		}
	}
	if imp.PlanDigest == rep.Digest {
		t.Errorf("tampered plan digest %s equals the fresh one; test setup wrong", imp.PlanDigest)
	}
	if _, err := Import(ctx, ImportOptions{Scan: opts, PlanFile: planFile, TargetRoot: t.TempDir(), Force: true}); err != nil {
		t.Errorf("--force should override the digest mismatch: %v", err)
	}
}

// (5b) Nonterminal legacy runs abort the import without --force and are
// listed by id.
func TestImportNonterminalAborts(t *testing.T) {
	ctx := context.Background()
	opts := testScanOptions(t)
	rep := scanFixture(t)
	planFile, _ := writePlan(t, rep)
	target := t.TempDir()

	imp, err := Import(ctx, ImportOptions{Scan: opts, PlanFile: planFile, TargetRoot: target, Force: false})
	if err == nil {
		t.Fatal("import with a nonterminal legacy run should abort")
	}
	if !strings.Contains(err.Error(), runRunning) {
		t.Errorf("abort error should name the nonterminal run: %v", err)
	}
	if len(imp.NonterminalAborted) != 1 || imp.NonterminalAborted[0] != runRunning {
		t.Errorf("nonterminal ids: %v (want [%s])", imp.NonterminalAborted, runRunning)
	}
	if _, err := os.Stat(filepath.Join(target, "lmw.db")); !os.IsNotExist(err) {
		t.Errorf("aborted import should not have created the target db: %v", err)
	}
}
