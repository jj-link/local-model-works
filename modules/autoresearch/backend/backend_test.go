package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/moduleapi"
)

func TestSelectedSecretScopes(t *testing.T) {
	input := map[string]any{
		"provider":        map[string]any{"secret_name": "provider-main"},
		"fallbacks":       []any{map[string]any{"secret_name": "provider-main"}, map[string]any{"secret_name": "provider-backup"}},
		"ssh_secret_name": "spark-key",
	}
	got := selectedSecretScopes(input)
	sort.Strings(got)
	want := []string{"provider-backup", "provider-main", "spark-key"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %v, want %v", got, want)
		}
	}
}

func TestPublicAddressPolicy(t *testing.T) {
	private := []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "::1", "fe80::1"}
	for _, raw := range private {
		if isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted private address %s", raw)
		}
	}
	if !isPublicIP(netip.MustParseAddr("8.8.8.8")) {
		t.Error("rejected public address")
	}
	result := resolveSource(context.Background(), "url", "https://127.0.0.1/private")
	if result.Error != "autoresearch.source_private_address" {
		t.Fatalf("private source error = %q", result.Error)
	}
}

func TestPaperPathRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	sections := filepath.Join(root, "sections")
	if err := os.Mkdir(sections, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sections, "intro.tex"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := securePaperPath(root, "../secret", false); err == nil {
		t.Fatal("accepted traversal")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := securePaperPath(root, "escape/file.tex", false); err == nil {
		t.Fatal("accepted escaping symlink")
	}
	path, err := securePaperPath(root, "sections/intro.tex", true)
	if err != nil || filepath.Base(path) != "intro.tex" {
		t.Fatalf("safe path = %q, %v", path, err)
	}
}

type seekableMultipartFile struct{ *os.File }

func TestPDFMagicAndSize(t *testing.T) {
	dir := t.TempDir()
	valid, err := os.CreateTemp(dir, "valid-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := valid.Write([]byte("%PDF-1.4\nfixture")); err != nil {
		t.Fatal(err)
	}
	_, _ = valid.Seek(0, io.SeekStart)
	digest, size, err := copyPDF(seekableMultipartFile{valid}, filepath.Join(dir, "upload.pdf"))
	valid.Close()
	if err != nil || size == 0 || len(digest) != 64 {
		t.Fatalf("valid PDF = digest %q size %d err %v", digest, size, err)
	}
	invalid, _ := os.CreateTemp(dir, "invalid-*.pdf")
	_, _ = invalid.Write([]byte("not a pdf"))
	_, _ = invalid.Seek(0, io.SeekStart)
	if _, _, err := copyPDF(seekableMultipartFile{invalid}, filepath.Join(dir, "upload.pdf")); err == nil || err.Error() != "autoresearch.source_pdf_invalid" {
		t.Fatalf("invalid PDF error = %v", err)
	}
	invalid.Close()
}

func TestParseIntakeCandidateRequiresExactHeadingOrder(t *testing.T) {
	valid := "# Grounded idea\n\n## Research question\nQuestion\n## Motivation\nMotivation\n## Proposed mechanism\nMechanism\n## Supporting sources\nhttps://example.test\n## Falsifiable claims\nClaim\n## Risks and disconfirming evidence\nRisk\n"
	candidate, err := parseIntakeCandidate([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Title != "Grounded idea" || !strings.Contains(candidate.Body, "## Falsifiable claims") {
		t.Fatalf("candidate = %#v", candidate)
	}
	invalid := strings.Replace(valid, "## Motivation", "## Unknown", 1)
	if _, err := parseIntakeCandidate([]byte(invalid)); err == nil || !strings.Contains(err.Error(), "## Motivation") {
		t.Fatalf("invalid candidate error = %v", err)
	}
}

func TestProjectHumanGateDefaultsAndOverrides(t *testing.T) {
	project := db.AutoresearchProject{ConfigJson: `{}`}
	if !projectHumanGate(project, "idea_selection") || !projectHumanGate(project, "paper_post_edit") {
		t.Fatal("default required gates are disabled")
	}
	if projectHumanGate(project, "experiment_handback") {
		t.Fatal("default experiment handback gate is enabled")
	}
	project.ConfigJson = `{"human_gates":{"paper_post_edit":false,"experiment_handback":true}}`
	if projectHumanGate(project, "paper_post_edit") || !projectHumanGate(project, "experiment_handback") {
		t.Fatal("project gate overrides were not applied")
	}
}

func TestMergeProjectDefaultsPreservesOverrides(t *testing.T) {
	config := map[string]any{
		"roles": map[string]any{"paper-writer": map[string]any{"model": "project"}},
	}
	settings := workerSettings{
		DefaultRoles: map[string]any{
			"default":      map[string]any{"model": "module"},
			"paper-writer": map[string]any{"model": "module-writer"},
		},
		DefaultAdvisors: map[string]any{"default": map[string]any{"enabled": false, "backlog": 1}},
	}
	mergeProjectDefaults(config, settings)
	roles := config["roles"].(map[string]any)
	if roles["paper-writer"].(map[string]any)["model"] != "project" {
		t.Fatal("project role override was replaced")
	}
	if roles["default"].(map[string]any)["model"] != "module" {
		t.Fatal("module default role was not applied")
	}
	advisors := config["advisors"].(map[string]any)
	if advisors["default"].(map[string]any)["enabled"] != false {
		t.Fatal("module advisor defaults were not applied")
	}
}

func TestPaperStatePhaseAndChangedDigests(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "PAPER_STATE.md")
	if err := os.WriteFile(state, []byte("---\nphase: needs_experiment\nround: 1\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	phase, err := paperPhase(state)
	if err != nil || phase != "needs_experiment" {
		t.Fatalf("phase = %q, %v", phase, err)
	}
	before := map[string]string{"main.tex": "old", "removed.tex": "removed"}
	after := map[string]string{"main.tex": "new", "created.tex": "created"}
	changed := changedDigestPaths(before, after)
	want := []string{"created.tex", "main.tex", "removed.tex"}
	if strings.Join(changed, ",") != strings.Join(want, ",") {
		t.Fatalf("changed = %v", changed)
	}
	if got := filterDigests(before, changed); len(got) != 2 || got["main.tex"] != "old" {
		t.Fatalf("before digests = %v", got)
	}
}

func TestImportGeneratedCandidatesReplacesUnselectedModelIdeas(t *testing.T) {
	module, projectID, _ := newTestModule(t)
	ctx := context.Background()
	if err := module.env.Q.CreateAutoResearchIdea(ctx, db.CreateAutoResearchIdeaParams{
		ID: uuid.NewString(), ProjectID: projectID, Ordinal: 1, Source: "generated", Title: "Old", Body: "Old", Selected: 0,
	}); err != nil {
		t.Fatal(err)
	}
	intake := filepath.Join(module.projectRoot(projectID), "artifacts", ".lmw", "intake")
	if err := os.MkdirAll(intake, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := "# New candidate\n\n## Research question\nQuestion\n## Motivation\nMotivation\n## Proposed mechanism\nMechanism\n## Supporting sources\nSource\n## Falsifiable claims\nClaim\n## Risks and disconfirming evidence\nRisk\n"
	if err := os.WriteFile(filepath.Join(intake, "candidate-001.md"), []byte(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intake, "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := module.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := module.importGeneratedCandidates(ctx, project, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed paths = %v", changed)
	}
	ideas, err := module.env.Q.ListAutoResearchIdeas(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 1 || ideas[0].Title != "New candidate" || ideas[0].Source != "generated" {
		t.Fatalf("ideas = %#v", ideas)
	}
	updated, err := module.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "awaiting_idea_selection" {
		t.Fatalf("status = %s", updated.Status)
	}
	if status, err := runGit(filepath.Join(module.projectRoot(projectID), "artifacts"), "status", "--porcelain"); err != nil || strings.TrimSpace(string(status)) != "" {
		t.Fatalf("artifact status = %q, %v", status, err)
	}
}

func newTestModule(t *testing.T) (*Module, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.New(database)
	projectID, err := id.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateAutoResearchProject(ctx, db.CreateAutoResearchProjectParams{
		ID: projectID, Name: "Fixture", Status: "paper_editing", ConfigJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{Q: queries, DB: database, AutoResearchRoot: filepath.Join(root, "autoresearch")}}
	projectRoot := module.projectRoot(projectID)
	if err := initializeProjectRoot(projectRoot); err != nil {
		t.Fatal(err)
	}
	paperRoot := module.paperRoot(projectID)
	if err := os.MkdirAll(filepath.Join(paperRoot, "sections"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paperRoot, "sections", "intro.tex")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(projectRoot, "artifacts")
	relative, _ := filepath.Rel(artifacts, path)
	if err := commitArtifacts(artifacts, "paper: fixture", filepath.ToSlash(relative)); err != nil {
		t.Fatal(err)
	}
	return module, projectID, path
}

func TestPaperETagConflictDoesNotOverwrite(t *testing.T) {
	module, projectID, path := newTestModule(t)
	parsed := uuid.MustParse(projectID)
	request := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString("after"))
	response := httptest.NewRecorder()
	module.UpdateAutoResearchPaperFile(response, request, parsed, "sections/intro.tex", UpdateAutoResearchPaperFileParams{IfMatch: `"wrong"`})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "paper.edit_conflict") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "before" {
		t.Fatalf("contents = %q, %v", contents, err)
	}
}

func TestSecretPurposesIncludeAutoResearch(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, purpose := range []string{"model_provider", "ssh"} {
		_, err := database.ExecContext(ctx, "INSERT INTO secrets(id,name,purpose,nonce,ciphertext) VALUES(?,?,?,?,?)", purpose, purpose, purpose, []byte{1}, []byte{2})
		if err != nil {
			t.Fatalf("insert %s secret: %v", purpose, err)
		}
	}
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE purpose IN ('model_provider','ssh')").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("secret count = %d", count)
	}
}

func TestWorkerSpecIsolation(t *testing.T) {
	spec := workerSpec(
		"run-1", "local/agon", "sha256:"+strings.Repeat("a", 64),
		"/state/autoresearch/project", "/state/autoresearch/project/scratch/run-1", "/state/autoresearch/project/scratch/run-1/credentials",
		"1000:1000", []string{"preflight"},
	)
	if !spec.ReadonlyRootfs || !spec.NoNewPrivileges || strings.Join(spec.CapDrop, ",") != "ALL" || spec.User != "1000:1000" {
		t.Fatalf("isolation flags = %+v", spec)
	}
	if spec.NetworkMode != "bridge" || spec.PidsLimit <= 0 || spec.MemoryBytes <= 0 || spec.CPU <= 0 || spec.TmpfsBytes <= 0 {
		t.Fatalf("resource bounds = %+v", spec)
	}
	if len(spec.Mounts) != 3 || spec.Mounts[0].ReadOnly || spec.Mounts[1].ReadOnly || !spec.Mounts[2].ReadOnly {
		t.Fatalf("mounts = %+v", spec.Mounts)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("docker.sock")) {
		t.Fatalf("Docker socket leaked into spec: %s", encoded)
	}
}

func TestWorkerImageMustBeDigestPinned(t *testing.T) {
	image, digest, err := imageParts("local/agon@sha256:" + strings.Repeat("b", 64))
	if err != nil || image != "local/agon" || digest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("image parts = %q %q %v", image, digest, err)
	}
	if _, _, err := imageParts("local/agon:latest"); err == nil {
		t.Fatal("accepted mutable worker image")
	}
}

func TestLMWProviderPreflightRequiresStreamingToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"lmw_probe\"}}\n\n"))
	}))
	defer server.Close()
	module := &Module{}
	if err := module.preflightLMWProvider(context.Background(), server.URL+"/v1", "fixture"); err != nil {
		t.Fatal(err)
	}

	incompatible := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{\"text\":\"no tools\"}`))
	}))
	defer incompatible.Close()
	if err := module.preflightLMWProvider(context.Background(), incompatible.URL+"/v1", "fixture"); err == nil || !strings.Contains(err.Error(), "provider_incompatible") {
		t.Fatalf("incompatible error = %v", err)
	}
}

func TestProjectWorkerUsesNonRootOwner(t *testing.T) {
	root := t.TempDir()
	user, err := projectWorkerUser(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(user, "0:") || user == "" {
		t.Fatalf("worker user = %q", user)
	}
}
