package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
)

const benchmarkAPIRunID = "01900000-0000-7000-8000-000000000010"

func newBenchmarkAPIHarness(t *testing.T) (*Module, http.Handler, *db.Queries, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.New(database)
	// Copy the read-only checked-in fixtures into a per-test isolated
	// directory under the gitignored repository-local .test-work root so
	// mutating tests never touch source-controlled testdata and repeated
	// runs stay order-independent. Removed again in cleanup.
	runRoot, err := isolationRoot(t, "benchmark-api")
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewEventBus(queries)
	runsService := runs.New(database, queries, bus, runRoot)
	jobRegistry := jobs.New(runsService, runRoot, ctx, database, queries)
	module := &Module{env: &moduleapi.Env{
		DB: database, Q: queries, Runs: runsService, Jobs: jobRegistry, RunRoot: runRoot,
	}}
	router := chi.NewRouter()
	module.RegisterHTTP(router)
	return module, router, queries, database, runRoot
}

// isolationRoot creates a unique project-local work directory under the
// gitignored .test-work root, copies the package's read-only testdata
// fixtures into it, and registers cleanup that removes just that
// directory. The returned path stands in for testdata as the mutable
// RunRoot so tests may create/delete files freely.
func isolationRoot(t *testing.T, name string) (string, error) {
	t.Helper()
	abs, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	root := filepath.Join(abs, "..", "..", "..", ".test-work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(abs, "testdata"))); err != nil {
		return "", err
	}
	return dir, nil
}

func insertBenchmarkAPIRun(t *testing.T, queries *db.Queries, runID, module, state string) {
	t.Helper()
	ctx := context.Background()
	if err := queries.CreateRun(ctx, db.CreateRunParams{
		ID: runID, Module: module, Kind: "benchmark", State: state, Resources: "{}", Input: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if module != descriptor.ID {
		return
	}
	if err := queries.InsertBenchmarkRunResult(ctx, db.InsertBenchmarkRunResultParams{
		RunID: runID, BenchmarkID: "terminal-bench", BenchmarkVersion: "3.0.0",
		Harness: "oracle", TaskCount: 1, CandidateCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func completeBenchmarkAPIBundle(t *testing.T, queries *db.Queries, runID, artifactID, metadataRunID, path string) {
	t.Helper()
	ctx := context.Background()
	metadata, err := json.Marshal(map[string]string{"run_id": metadataRunID, "path": path})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: artifactID, Kind: "benchmark-result-bundle", Identity: "file://" + artifactID,
		Digest:   sql.NullString{String: "sha256:efb09c96bae1b7eade745e347f1b14c3f823e569fbaea0b7d195141795f2a08d", Valid: true},
		Metadata: string(metadata),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CompleteBenchmarkRunResult(ctx, db.CompleteBenchmarkRunResultParams{
		BundleArtifactID: sql.NullString{String: artifactID, Valid: true}, MetricsJson: "{}", RunID: runID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkBundleRequiresSucceededOwningRegularFile(t *testing.T) {
	_, router, queries, _, runRoot := newBenchmarkAPIHarness(t)
	bundlePath := filepath.Join(runRoot, "jobs", benchmarkAPIRunID, "result-bundle.tar.gz")
	insertBenchmarkAPIRun(t, queries, benchmarkAPIRunID, descriptor.ID, "succeeded")
	completeBenchmarkAPIBundle(t, queries, benchmarkAPIRunID, "artifact-good", benchmarkAPIRunID, bundlePath)

	request := httptest.NewRequest(http.MethodGet, "/benchmarks/"+benchmarkAPIRunID+"/bundle", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "benchmark bundle fixture\n" {
		t.Fatalf("valid bundle: status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != `"sha256:efb09c96bae1b7eade745e347f1b14c3f823e569fbaea0b7d195141795f2a08d"` ||
		response.Header().Get("Content-Length") != "25" {
		t.Fatalf("bundle integrity headers = %q %q", response.Header().Get("ETag"), response.Header().Get("Content-Length"))
	}

	crossRunID := "01900000-0000-7000-8000-000000000011"
	insertBenchmarkAPIRun(t, queries, crossRunID, descriptor.ID, "succeeded")
	completeBenchmarkAPIBundle(t, queries, crossRunID, "artifact-cross", benchmarkAPIRunID, bundlePath)
	request = httptest.NewRequest(http.MethodGet, "/benchmarks/"+crossRunID+"/bundle", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-run bundle status = %d, want 404", response.Code)
	}

	runningRunID := "01900000-0000-7000-8000-000000000012"
	insertBenchmarkAPIRun(t, queries, runningRunID, descriptor.ID, "running")
	request = httptest.NewRequest(http.MethodGet, "/benchmarks/"+runningRunID+"/bundle", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("running bundle status = %d, want 409", response.Code)
	}
}

func TestReconcileInterruptedRunsScansOwnershipOnlyInterruptedWorkspaces(t *testing.T) {
	module, _, queries, _, runRoot := newBenchmarkAPIHarness(t)
	// No member runner node is enrolled in this harness, so availability
	// resolution fails: reconciliation must count 0 dispatched cleanups.
	interrupted := "01900000-0000-7000-8000-000000000021"
	insertBenchmarkAPIRun(t, queries, interrupted, descriptor.ID, "interrupted")
	succeeded := "01900000-0000-7000-8000-000000000022"
	insertBenchmarkAPIRun(t, queries, succeeded, descriptor.ID, "succeeded")
	noManifest := "01900000-0000-7000-8000-000000000023"
	insertBenchmarkAPIRun(t, queries, noManifest, descriptor.ID, "interrupted")

	manifest := `{"schema_version":1,"run_id":"` + interrupted + `","project_prefix":"lmw-` + interrupted + `","containers":[],"networks":[],"volumes":[]}`
	for _, runID := range []string{interrupted, succeeded} {
		if err := os.MkdirAll(filepath.Join(runRoot, "jobs", runID), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runRoot, "jobs", runID, "ownership.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	module.overrideCleanupDispatch = func(ctx context.Context, runID string) error { return errors.New("dispatch unavailable in test") }

	processed := module.ReconcileInterruptedRuns(context.Background())
	if processed != 0 {
		t.Fatalf("processed = %d, want 0 when dispatch fails", processed)
	}
	// succeeded run is never scanned; noManifest run is skipped.
	if _, err := os.Stat(filepath.Join(runRoot, "jobs", succeeded, "ownership.json")); err != nil {
		t.Fatalf("succeeded run ownership deleted: %v", err)
	}
}

func TestBenchmarkTrialsAndCancellationAreModuleScoped(t *testing.T) {
	_, router, queries, _, _ := newBenchmarkAPIHarness(t)
	insertBenchmarkAPIRun(t, queries, benchmarkAPIRunID, descriptor.ID, "succeeded")
	if err := queries.InsertBenchmarkTrialResult(context.Background(), db.InsertBenchmarkTrialResultParams{
		RunID: benchmarkAPIRunID, TaskID: "html-js-filter", CandidateIndex: 0,
		OfficialPass: 1, VerifierSelected: 1, TrajectoryPath: "harbor/trial/agent/trajectory.json", MetricsJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/benchmarks/"+benchmarkAPIRunID+"/trials", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("trials status = %d: %s", response.Code, response.Body.String())
	}
	var trials []benchmarkTrialView
	if err := json.Unmarshal(response.Body.Bytes(), &trials); err != nil {
		t.Fatal(err)
	}
	if len(trials) != 1 || !trials[0].OfficialPass || !trials[0].VerifierSelected {
		t.Fatalf("trials = %#v", trials)
	}

	queuedRunID := "01900000-0000-7000-8000-000000000013"
	insertBenchmarkAPIRun(t, queries, queuedRunID, descriptor.ID, "queued")
	request = httptest.NewRequest(http.MethodPost, "/benchmarks/"+queuedRunID+"/cancel", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d: %s", response.Code, response.Body.String())
	}

	foreignRunID := "01900000-0000-7000-8000-000000000014"
	insertBenchmarkAPIRun(t, queries, foreignRunID, "autoresearch", "queued")
	request = httptest.NewRequest(http.MethodPost, "/benchmarks/"+foreignRunID+"/cancel", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("foreign cancel status = %d, want 404", response.Code)
	}
}

func TestServiceTokenCanReadAllButCancelOnlyItsOwnBenchmark(t *testing.T) {
	_, router, queries, database, _ := newBenchmarkAPIHarness(t)
	ownedRunID := "01900000-0000-7000-8000-000000000031"
	otherRunID := "01900000-0000-7000-8000-000000000032"
	insertBenchmarkAPIRun(t, queries, ownedRunID, descriptor.ID, "queued")
	insertBenchmarkAPIRun(t, queries, otherRunID, descriptor.ID, "queued")
	ownedInput := `{"origin":{"client_id":"01900000-0000-7000-8000-000000000071","client_name":"factory","run_id":"01900000-0000-7000-8000-000000000081","project_id":"01900000-0000-7000-8000-000000000082"}}`
	otherInput := `{"origin":{"client_id":"01900000-0000-7000-8000-000000000072","client_name":"other"}}`
	if _, err := database.Exec(`UPDATE runs SET input=? WHERE id=?`, ownedInput, ownedRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE runs SET input=? WHERE id=?`, otherInput, otherRunID); err != nil {
		t.Fatal(err)
	}
	principal := &auth.APITokenPrincipal{ID: "01900000-0000-7000-8000-000000000071", Name: "factory"}
	request := httptest.NewRequest(http.MethodGet, "/benchmarks/"+ownedRunID, nil)
	request = request.WithContext(auth.ContextWithAPITokenPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"client_name":"factory"`) {
		t.Fatalf("owned get status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/benchmarks/"+otherRunID, nil)
	request = request.WithContext(auth.ContextWithAPITokenPrincipal(request.Context(), principal))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"client_name":"other"`) {
		t.Fatalf("cross-owner read status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/benchmarks/"+otherRunID+"/cancel", nil)
	request = request.WithContext(auth.ContextWithAPITokenPrincipal(request.Context(), principal))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign cancel status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/benchmarks/"+ownedRunID+"/cancel", nil)
	request = request.WithContext(auth.ContextWithAPITokenPrincipal(request.Context(), principal))
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("owned cancel status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestBenchmarkSummaryDownloadPublishesRegisteredIntegrity(t *testing.T) {
	_, router, queries, _, runRoot := newBenchmarkAPIHarness(t)
	runID := "01900000-0000-7000-8000-000000000041"
	insertBenchmarkAPIRun(t, queries, runID, descriptor.ID, "succeeded")
	path := filepath.Join(runRoot, "jobs", runID, "summary.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"ok\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]string{"run_id": runID, "path": path})
	if err := queries.CreateArtifact(context.Background(), db.CreateArtifactParams{
		ID: "artifact-summary", Kind: "benchmark-summary", Identity: "file://summary",
		Digest:   sql.NullString{String: "sha256:e5f1eb4d806641698a35efe20e098efd20d7d57a9b90ee69079d5bb650920726", Valid: true},
		Metadata: string(metadata),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CompleteBenchmarkRunResult(context.Background(), db.CompleteBenchmarkRunResultParams{
		SummaryArtifactID: sql.NullString{String: "artifact-summary", Valid: true}, MetricsJson: "{}", RunID: runID,
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/benchmarks/"+runID+"/summary", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("summary status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("ETag") != `"sha256:e5f1eb4d806641698a35efe20e098efd20d7d57a9b90ee69079d5bb650920726"` ||
		response.Header().Get("Content-Length") != "12" {
		t.Fatalf("summary integrity headers = %q %q", response.Header().Get("ETag"), response.Header().Get("Content-Length"))
	}
}

func TestApplyBenchmarkOriginStampsAuthenticatedIdentityAndRejectsBrowserOrigin(t *testing.T) {
	req := benchmarkCreate{Origin: &benchmarkOriginInput{
		RunID:     "01900000-0000-7000-8000-000000000081",
		ProjectID: "01900000-0000-7000-8000-000000000082",
	}}
	if err := applyBenchmarkOrigin(context.Background(), req, map[string]any{}); err == nil {
		t.Fatal("browser-supplied service origin was accepted")
	}
	principal := &auth.APITokenPrincipal{
		ID: "01900000-0000-7000-8000-000000000071", Name: "factory",
	}
	ctx := auth.ContextWithAPITokenPrincipal(context.Background(), principal)
	input := map[string]any{"origin": map[string]any{"client_id": "spoofed", "client_name": "spoofed"}}
	if err := applyBenchmarkOrigin(ctx, req, input); err != nil {
		t.Fatal(err)
	}
	origin, ok := input["origin"].(map[string]any)
	if !ok || origin["client_id"] != principal.ID || origin["client_name"] != principal.Name ||
		origin["run_id"] != req.Origin.RunID || origin["project_id"] != req.Origin.ProjectID {
		t.Fatalf("stamped origin = %#v", input["origin"])
	}
	withoutParent := map[string]any{}
	if err := applyBenchmarkOrigin(ctx, benchmarkCreate{}, withoutParent); err != nil {
		t.Fatal(err)
	}
	serviceOrigin := withoutParent["origin"].(map[string]any)
	if serviceOrigin["client_id"] != principal.ID {
		t.Fatalf("service-only origin = %#v", serviceOrigin)
	}
	if _, hasParent := serviceOrigin["run_id"]; hasParent {
		t.Fatalf("service-only origin contains parent IDs: %#v", serviceOrigin)
	}
}
