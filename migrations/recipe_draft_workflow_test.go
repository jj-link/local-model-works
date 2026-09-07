package migrations

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRecipeDraftWorkflowMigrationPreservesAndExpandsLegacyDraft(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	base, err := FS.ReadFile("006_recipe_drafts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(base)); err != nil {
		t.Fatal(err)
	}
	candidates := `[{"path":"a.bin","size":3,"sha256":"same"},{"path":"nested/b.bin","size":3,"sha256":"same"},{"path":"other.bin","size":4,"sha256":"other"}]`
	if _, err := database.Exec(`INSERT INTO recipe_drafts
		(id, version, state, source, resolved_commit, resolved_tree, manifest, candidates, selected_assets, diagnostics, package_digest)
		VALUES ('draft-1', 7, 'packaged', '{"remote":"https://github.com/o/r"}', 'commit', 'tree', '{"name":"kept"}', ?, '["same","missing"]', '[{"code":"existing","message":"kept"}]', 'sha256:package')`, candidates); err != nil {
		t.Fatal(err)
	}
	migration, err := FS.ReadFile("017_recipe_draft_workflow.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}

	var version int64
	var state, source, manifest, migratedCandidates, selected, diagnostics string
	var operation, proposal, parent sql.NullString
	var contextSelection, questions, warnings, references string
	if err := database.QueryRow(`SELECT version,state,source,manifest,candidates,selected_assets,diagnostics,
		operation,proposal,context_selection,questions,acknowledged_warnings,resolved_references,parent_draft_id
		FROM recipe_drafts WHERE id='draft-1'`).Scan(&version, &state, &source, &manifest, &migratedCandidates, &selected, &diagnostics,
		&operation, &proposal, &contextSelection, &questions, &warnings, &references, &parent); err != nil {
		t.Fatal(err)
	}
	if version != 7 || state != "packaged" || source != `{"remote":"https://github.com/o/r"}` || manifest != `{"name":"kept"}` {
		t.Fatalf("legacy identity/content changed: version=%d state=%s source=%s manifest=%s", version, state, source, manifest)
	}
	if operation.Valid || proposal.Valid || parent.Valid || contextSelection != "[]" || questions != "[]" || warnings != "[]" || references != "[]" {
		t.Fatal("new nullable/default workflow fields were not initialized")
	}
	var gotCandidates []map[string]any
	if err := json.Unmarshal([]byte(migratedCandidates), &gotCandidates); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range gotCandidates {
		if candidate["origin"] != "source" {
			t.Fatalf("candidate origin = %#v", candidate["origin"])
		}
	}
	var gotSelections []map[string]string
	if err := json.Unmarshal([]byte(selected), &gotSelections); err != nil {
		t.Fatal(err)
	}
	if len(gotSelections) != 2 || gotSelections[0]["path"] != "a.bin" || gotSelections[1]["path"] != "nested/b.bin" {
		t.Fatalf("legacy identical-byte selection was not expanded by path: %#v", gotSelections)
	}
	var gotDiagnostics []map[string]any
	if err := json.Unmarshal([]byte(diagnostics), &gotDiagnostics); err != nil {
		t.Fatal(err)
	}
	if len(gotDiagnostics) != 2 || gotDiagnostics[0]["code"] != "existing" || gotDiagnostics[1]["code"] != "recipe.asset_selection_missing" || gotDiagnostics[1]["blocking"] != true {
		t.Fatalf("legacy diagnostic or missing-selection blocker lost: %#v", gotDiagnostics)
	}
}
