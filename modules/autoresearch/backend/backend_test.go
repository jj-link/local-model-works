package backend

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/nodes"
	runsvc "github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
	"github.com/jj-link/local-model-works/migrations"
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

func TestParseArxivFeed(t *testing.T) {
	feed := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <id>https://arxiv.org/abs/2608.12345v2</id>
    <updated>2026-08-20T12:00:00Z</updated>
    <published>2026-08-18T12:00:00Z</published>
    <title>  Sparse latent
      world models  </title>
    <summary> Long-horizon planning with sparse state. </summary>
    <author><name>Ada Researcher</name></author>
    <author><name>Grace Scientist</name></author>
    <link href="https://arxiv.org/abs/2608.12345v2" rel="alternate" type="text/html"/>
    <link title="pdf" href="https://arxiv.org/pdf/2608.12345v2" rel="related" type="application/pdf"/>
  </entry>
</feed>`)
	resolved, err := parseArxivFeed(feed, "2608.12345")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Title != "Sparse latent world models" || resolved.Status != "ready" {
		t.Fatalf("resolved = %#v", resolved)
	}
	authors, ok := resolved.Metadata["authors"].([]string)
	if !ok || len(authors) != 2 || authors[1] != "Grace Scientist" {
		t.Fatalf("authors = %#v", resolved.Metadata["authors"])
	}
	if resolved.Metadata["pdf_url"] != "https://arxiv.org/pdf/2608.12345v2" {
		t.Fatalf("pdf_url = %#v", resolved.Metadata["pdf_url"])
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
		"roles":    map[string]any{"paper-writer": map[string]any{"model": "project"}},
		"advisors": map[string]any{"paper-writer": map[string]any{"enabled": true, "backlog": 1}},
	}
	settings := workerSettings{
		DefaultRoles: map[string]any{
			"default":      map[string]any{"model": "module"},
			"paper-writer": map[string]any{"model": "module-writer"},
		},
		DefaultAdvisors: map[string]any{"default": map[string]any{"enabled": false, "backlog": 1, "provider": map[string]any{"model": "advisor"}}},
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
	if advisors["paper-writer"].(map[string]any)["provider"].(map[string]any)["model"] != "advisor" {
		t.Fatal("enabled advisor did not inherit the configured default provider")
	}
}

func newRunnerSettingsModule(t *testing.T, values map[string]any) *Module {
	t.Helper()
	ctx := context.Background()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(ctx, database, migrations.FS); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database)
	registry := settings.New(queries)
	if err := registry.Register(descriptor.ID, descriptor.SettingsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Set(ctx, descriptor.ID, values, "0"); err != nil {
		t.Fatal(err)
	}
	return &Module{env: &moduleapi.Env{
		Q: queries, DB: database, Settings: registry, Nodes: nodes.NewRegistry(),
	}}
}

func runnerTestSettings() map[string]any {
	return map[string]any{
		"worker_image": "local/agon@sha256:" + strings.Repeat("a", 64),
	}
}

func addRunnerNodeWithInventory(
	t *testing.T,
	module *Module,
	inventory sql.NullString,
	approved bool,
	online bool,
) string {
	t.Helper()
	ctx := context.Background()
	nodeID := uuid.NewString()
	if err := module.env.Q.CreateNode(ctx, db.CreateNodeParams{
		ID: nodeID, DisplayName: nodeID, Labels: "{}", CreatedAt: nodeID,
	}); err != nil {
		t.Fatal(err)
	}
	if inventory.Valid {
		if err := module.env.Q.SetNodeInventory(ctx, db.SetNodeInventoryParams{
			ID: nodeID, Inventory: inventory,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if approved {
		if err := module.env.Q.ApproveNode(ctx, nodeID); err != nil {
			t.Fatal(err)
		}
	}
	if online {
		registry := module.env.Nodes
		connection := nodes.NewConn(nodeID)
		registry.Register(connection)
		t.Cleanup(func() {
			registry.Unregister(connection)
			connection.Close()
		})
	}
	return nodeID
}

func addRunnerNode(t *testing.T, module *Module, hostname string, approved, online bool) string {
	t.Helper()
	raw, err := json.Marshal(runnerNodeInventory{Hostname: hostname})
	if err != nil {
		t.Fatal(err)
	}
	return addRunnerNodeWithInventory(
		t,
		module,
		sql.NullString{String: string(raw), Valid: true},
		approved,
		online,
	)
}

func localHostname(t *testing.T) string {
	t.Helper()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return hostname
}

func TestLocalRunnerResolvesApprovedOnlineNode(t *testing.T) {
	module := newRunnerSettingsModule(t, runnerTestSettings())
	nodeID := addRunnerNode(t, module, "  "+strings.ToUpper(localHostname(t))+"  ", true, true)

	got, err := module.requireWorkerSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerNodeID != nodeID {
		t.Fatalf("runner node = %q, want %q", got.RunnerNodeID, nodeID)
	}
}

func TestExplicitRunnerOverridesLocalCandidate(t *testing.T) {
	explicitNodeID := uuid.NewString()
	values := runnerTestSettings()
	values["runner_node_id"] = explicitNodeID
	module := newRunnerSettingsModule(t, values)
	_ = addRunnerNode(t, module, localHostname(t), true, true)
	connection := nodes.NewConn(explicitNodeID)
	module.env.Nodes.Register(connection)
	t.Cleanup(func() {
		module.env.Nodes.Unregister(connection)
		connection.Close()
	})

	got, err := module.requireWorkerSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerNodeID != explicitNodeID {
		t.Fatalf("runner node = %q, want explicit %q", got.RunnerNodeID, explicitNodeID)
	}
}

func TestLocalRunnerRejectsRemoteOnlyNodes(t *testing.T) {
	module := newRunnerSettingsModule(t, runnerTestSettings())
	_ = addRunnerNode(t, module, localHostname(t)+".remote", true, true)

	_, err := module.requireWorkerSettings(context.Background())
	const want = "autoresearch.runner_not_configured: no approved local runner is registered"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestLocalRunnerReportsApprovedOfflineNode(t *testing.T) {
	module := newRunnerSettingsModule(t, runnerTestSettings())
	_ = addRunnerNode(t, module, localHostname(t), true, false)
	module.env.Nodes = nil

	_, err := module.requireWorkerSettings(context.Background())
	if err == nil || err.Error() != "autoresearch.runner_offline" {
		t.Fatalf("error = %v, want autoresearch.runner_offline", err)
	}
}

func TestLocalRunnerRejectsAmbiguousOnlineNodes(t *testing.T) {
	module := newRunnerSettingsModule(t, runnerTestSettings())
	hostname := localHostname(t)
	_ = addRunnerNode(t, module, hostname, true, true)
	_ = addRunnerNode(t, module, hostname, true, true)

	_, err := module.requireWorkerSettings(context.Background())
	const want = "autoresearch.runner_not_configured: multiple approved local runners match this host"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestLocalRunnerIgnoresPendingAndInvalidInventory(t *testing.T) {
	module := newRunnerSettingsModule(t, runnerTestSettings())
	_ = addRunnerNode(t, module, localHostname(t), false, true)
	_ = addRunnerNodeWithInventory(
		t,
		module,
		sql.NullString{String: "{not-json", Valid: true},
		true,
		true,
	)
	_ = addRunnerNodeWithInventory(t, module, sql.NullString{}, true, true)

	_, err := module.requireWorkerSettings(context.Background())
	const want = "autoresearch.runner_not_configured: no approved local runner is registered"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestWriteIdeaIntakeInputsMaterializesPromptAndManifest(t *testing.T) {
	module, projectID, _ := newTestModule(t)
	inputs, err := module.writeIdeaIntakeInputs(context.Background(), projectID, module.projectRoot(projectID), " bounded prompt ")
	if err != nil {
		t.Fatal(err)
	}
	if inputs["topic_prompt_file"] != "/project/.lmw/inputs/topic-prompt.txt" ||
		inputs["source_manifest"] != "/project/.lmw/inputs/source-manifest.json" {
		t.Fatalf("inputs = %v", inputs)
	}
	prompt, err := os.ReadFile(filepath.Join(module.projectRoot(projectID), ".lmw", "inputs", "topic-prompt.txt"))
	if err != nil || string(prompt) != "bounded prompt\n" {
		t.Fatalf("prompt = %q, %v", prompt, err)
	}
	manifest, err := os.ReadFile(filepath.Join(module.projectRoot(projectID), ".lmw", "inputs", "source-manifest.json"))
	if err != nil || strings.TrimSpace(string(manifest)) != "[]" {
		t.Fatalf("manifest = %q, %v", manifest, err)
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

func TestImportGeneratedCandidatesPreservesHumanAndSelectedIdeas(t *testing.T) {
	module, projectID, _ := newTestModule(t)
	ctx := context.Background()
	for _, idea := range []db.CreateAutoResearchIdeaParams{
		{ID: uuid.NewString(), ProjectID: projectID, Ordinal: 0, Source: "human", Title: "Human", Body: "Human", Selected: 0},
		{ID: uuid.NewString(), ProjectID: projectID, Ordinal: 1, Source: "generated", Title: "Old", Body: "Old", Selected: 0},
		{ID: uuid.NewString(), ProjectID: projectID, Ordinal: 2, Source: "generated", Title: "Chosen", Body: "Chosen", Selected: 1},
	} {
		if err := module.env.Q.CreateAutoResearchIdea(ctx, idea); err != nil {
			t.Fatal(err)
		}
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
	if len(ideas) != 3 || ideas[0].Title != "Human" || ideas[1].Title != "Chosen" || ideas[2].Title != "New candidate" {
		t.Fatalf("ideas = %#v", ideas)
	}
	if ideas[0].Ordinal != 0 || ideas[2].Ordinal != 3 {
		t.Fatalf("ordinals = %d, %d, want 0, 3", ideas[0].Ordinal, ideas[2].Ordinal)
	}
	selected := 0
	for _, idea := range ideas {
		selected += int(idea.Selected)
	}
	if selected != 1 || ideas[1].Selected != 1 {
		t.Fatalf("selection state = %#v", ideas)
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

func TestCreateProjectThenImportTenGeneratedAlternatives(t *testing.T) {
	module, _, _ := newTestModule(t)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"idea_prompt":"Can ten alternatives coexist with the human idea?"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	module.CreateAutoResearchProject(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create response = %d %s", response.Code, response.Body.String())
	}
	projects, err := module.env.Q.ListAutoResearchProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var project db.AutoresearchProject
	for _, candidate := range projects {
		if candidate.IdeaPrompt == "Can ten alternatives coexist with the human idea?" {
			project = candidate
			break
		}
	}
	if project.ID == "" {
		t.Fatal("created project not found")
	}
	intake := filepath.Join(module.projectRoot(project.ID), "artifacts", ".lmw", "intake")
	if err := os.MkdirAll(intake, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 10; index++ {
		candidate := fmt.Sprintf("# Candidate %d\n\n## Research question\nQuestion\n## Motivation\nMotivation\n## Proposed mechanism\nMechanism\n## Supporting sources\nSource\n## Falsifiable claims\nClaim\n## Risks and disconfirming evidence\nRisk\n", index)
		path := filepath.Join(intake, fmt.Sprintf("candidate-%03d.md", index))
		if err := os.WriteFile(path, []byte(candidate), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := module.importGeneratedCandidates(context.Background(), project, 10); err != nil {
		t.Fatal(err)
	}
	ideas, err := module.env.Q.ListAutoResearchIdeas(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 11 || ideas[0].Source != "human" || ideas[0].Ordinal != 0 {
		t.Fatalf("ideas = %#v", ideas)
	}
	selected := 0
	for _, idea := range ideas {
		selected += int(idea.Selected)
	}
	if selected != 1 {
		t.Fatalf("selected count = %d, want 1", selected)
	}
	if status, err := runGit(filepath.Join(module.projectRoot(project.ID), "artifacts"), "status", "--porcelain"); err != nil || strings.TrimSpace(string(status)) != "" {
		t.Fatalf("artifact status = %q, %v", status, err)
	}
}

func TestSelectedIdeaSynchronizesCanonicalArtifactsOnSelectAndEdit(t *testing.T) {
	module, projectID, _ := newTestModule(t)
	ctx := context.Background()
	humanID := uuid.NewString()
	generatedID := uuid.NewString()
	for _, idea := range []db.CreateAutoResearchIdeaParams{
		{ID: humanID, ProjectID: projectID, Ordinal: 0, Source: "human", Title: "Human idea", Body: "Human body", Selected: 1},
		{ID: generatedID, ProjectID: projectID, Ordinal: 1, Source: "generated", Title: "Generated idea", Body: "Generated body", Selected: 0},
	} {
		if err := module.env.Q.CreateAutoResearchIdea(ctx, idea); err != nil {
			t.Fatal(err)
		}
	}
	human, err := module.env.Q.GetAutoResearchIdea(ctx, db.GetAutoResearchIdeaParams{ID: humanID, ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncSelectedIdeaArtifacts(module.projectRoot(projectID), human); err != nil {
		t.Fatal(err)
	}

	selectRequest := httptest.NewRequest(http.MethodPost, "/", nil)
	selectResponse := httptest.NewRecorder()
	module.SelectAutoResearchIdea(selectResponse, selectRequest, uuid.MustParse(projectID), uuid.MustParse(generatedID))
	if selectResponse.Code != http.StatusOK {
		t.Fatalf("select response = %d %s", selectResponse.Code, selectResponse.Body.String())
	}
	indexPath := filepath.Join(module.projectRoot(projectID), "artifacts", "ideas", "ideas.xml")
	index, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	generatedSlug := artifactSlug("Generated idea", generatedID)
	if !strings.Contains(string(index), generatedSlug) || strings.Contains(string(index), artifactSlug("Human idea", humanID)) {
		t.Fatalf("canonical index = %s", index)
	}
	ideas, err := module.env.Q.ListAutoResearchIdeas(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	selected := 0
	var generated db.AutoresearchIdea
	for _, idea := range ideas {
		selected += int(idea.Selected)
		if idea.ID == generatedID {
			generated = idea
		}
	}
	if selected != 1 || generated.Selected != 1 {
		t.Fatalf("selection state = %#v", ideas)
	}

	updateRequest := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(`{"title":"Edited generated idea","body":"Edited canonical body"}`))
	updateRequest.Header.Set("Content-Type", "application/json")
	updateResponse := httptest.NewRecorder()
	module.UpdateAutoResearchIdea(updateResponse, updateRequest, uuid.MustParse(projectID), uuid.MustParse(generatedID), UpdateAutoResearchIdeaParams{
		IfMatch: strconv.FormatInt(generated.Version, 10),
	})
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update response = %d %s", updateResponse.Code, updateResponse.Body.String())
	}
	editedSlug := artifactSlug("Edited generated idea", generatedID)
	markdown, err := os.ReadFile(filepath.Join(module.projectRoot(projectID), "artifacts", "ideas", editedSlug+".v1.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(markdown), "Edited canonical body") {
		t.Fatalf("canonical markdown = %s", markdown)
	}
	if _, err := os.Stat(filepath.Join(module.projectRoot(projectID), "artifacts", "ideas", generatedSlug+".v1.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old selected artifact still exists: %v", err)
	}
	if status, err := runGit(filepath.Join(module.projectRoot(projectID), "artifacts"), "status", "--porcelain"); err != nil || strings.TrimSpace(string(status)) != "" {
		t.Fatalf("artifact status = %q, %v", status, err)
	}
}

func TestProjectStatusTransitionIgnoresInterveningConfigVersion(t *testing.T) {
	module, projectID, _ := newTestModule(t)
	ctx := context.Background()
	project, err := module.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := module.env.Q.UpdateAutoResearchProject(ctx, db.UpdateAutoResearchProjectParams{
		Name: project.Name, Status: project.Status, IdeaPrompt: project.IdeaPrompt,
		ConfigJson: `{"paper_max_rounds":7}`, ID: project.ID, Version: project.Version,
	})
	if err != nil || rows != 1 {
		t.Fatalf("config update = %d, %v", rows, err)
	}
	if err := module.setProjectStatus(ctx, projectID, "completed"); err != nil {
		t.Fatal(err)
	}
	updated, err := module.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "completed" || updated.Version != project.Version+2 {
		t.Fatalf("project after lifecycle transition = %#v", updated)
	}
}

func TestEffectiveProviderSnapshotAppliesRoleOverrides(t *testing.T) {
	config, err := effectiveProviderSnapshot(
		`{"roles":{"idea-creator":{"source":"external","backend":"codex","model":"old","secret_name":"old"}}}`,
		workerSettings{DefaultRoles: map[string]any{"default": map[string]any{"source": "external", "backend": "codex", "model": "default", "secret_name": "default"}}},
		map[string]any{"idea-creator": map[string]any{"source": "external", "backend": "claude", "model": "new", "secret_name": "new"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	roles := config["roles"].(map[string]any)
	if roles["idea-creator"].(map[string]any)["model"] != "new" || roles["default"].(map[string]any)["model"] != "default" {
		t.Fatalf("effective roles = %#v", roles)
	}
}

func TestCredentialCleanupAndSanitizedNameCollision(t *testing.T) {
	autoRoot := t.TempDir()
	projectID := uuid.NewString()
	runID := uuid.NewString()
	runScratch := filepath.Join(autoRoot, projectID, "scratch", runID)
	if err := os.MkdirAll(filepath.Join(runScratch, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runScratch, "ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runScratch, "credentials", "provider"), []byte("provider-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runScratch, "ssh", "id_key"), []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(runScratch, "transcript.ndjson"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scrubStartupCredentials(autoRoot); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(runScratch, "credentials"), filepath.Join(runScratch, "ssh", "id_key")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential path remains: %s: %v", path, err)
		}
	}
	if contents, err := os.ReadFile(filepath.Join(runScratch, "transcript.ndjson")); err != nil || string(contents) != "keep" {
		t.Fatalf("non-secret scratch changed: %q, %v", contents, err)
	}

	collisionRoot := filepath.Join(autoRoot, projectID, "scratch", uuid.NewString())
	if _, err := writeCredentialFiles(collisionRoot, map[string]string{"provider/a": "one", "provider?a": "two"}); err == nil || !strings.HasPrefix(err.Error(), "autoresearch.secret_name_collision") {
		t.Fatalf("collision error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(collisionRoot, "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision wrote credential directory: %v", err)
	}
}
func TestDirectPaperMutationRejectedWhileProjectLeaseIsActive(t *testing.T) {
	module, projectID, path := newTestModule(t)
	ctx := context.Background()
	runRoot := filepath.Join(filepath.Dir(module.env.AutoResearchRoot), "runs")
	module.env.Runs = runsvc.New(module.env.DB, module.env.Q, events.NewEventBus(module.env.Q), runRoot)
	runID, err := module.env.Runs.Create(ctx, "autoresearch", "autoresearch-factory", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := module.env.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := module.env.Runs.AcquireLeases(ctx, module.env.Q.WithTx(tx), "run", runID, []string{projectLeaseResource(projectID)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString("after"))
	response := httptest.NewRecorder()
	module.UpdateAutoResearchPaperFile(response, request, uuid.MustParse(projectID), "sections/intro.tex", UpdateAutoResearchPaperFileParams{
		IfMatch: digestBytes(before),
	})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "autoresearch.project_busy") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("paper changed while leased: %q, %v", after, err)
	}
}

func TestNextFactoryFollowsReadinessAndRetries(t *testing.T) {
	tests := []struct {
		name    string
		factory AutoResearchFactory
		state   string
		input   map[string]any
		want    AutoResearchFactory
	}{
		{name: "new idea tick advances", factory: Idea, state: "succeeded", want: Proposal},
		{name: "idea intake waits for selection", factory: Idea, state: "succeeded", input: map[string]any{"candidate_count": 3}, want: Idea},
		{name: "proposal advances to literature", factory: Proposal, state: "succeeded", want: DeepLit},
		{name: "failed experiment retries", factory: Experiment, state: "failed", want: Experiment},
		{name: "experiment advances to paper", factory: Experiment, state: "succeeded", want: Paper},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := nextFactoryForRun(test.factory, test.state, test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("factory = %s, want %s", got, test.want)
			}
		})
	}
}

func TestCreateProjectRequiresQuestionButNotName(t *testing.T) {
	module, _, _ := newTestModule(t)
	question := "Can sparse latent world models improve long-horizon planning?"
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"idea_prompt":`+strconv.Quote(question)+`}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	module.CreateAutoResearchProject(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var project AutoResearchProject
	if err := json.Unmarshal(response.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.Name != "" || project.IdeaPrompt != question || project.Status != "idea_intake" {
		t.Fatalf("project = %#v", project)
	}
	ideas, err := module.env.Q.ListAutoResearchIdeas(context.Background(), project.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 1 || ideas[0].Selected != 1 || ideas[0].Body != question {
		t.Fatalf("initial ideas = %#v", ideas)
	}
}

func TestCreateProjectPreservesManualName(t *testing.T) {
	module, _, _ := newTestModule(t)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"name":"  Manual workspace  ","idea_prompt":"A concrete research question"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	module.CreateAutoResearchProject(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var project AutoResearchProject
	if err := json.Unmarshal(response.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.Name != "Manual workspace" {
		t.Fatalf("name = %q", project.Name)
	}
}

func TestCreateProjectRejectsBlankQuestion(t *testing.T) {
	module, _, _ := newTestModule(t)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"idea_prompt":"  "}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	module.CreateAutoResearchProject(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "research question is required") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
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
