package migrations

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openBenchmarkMigrationFixture(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	base, err := FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(base)); err != nil {
		t.Fatal(err)
	}
	return database
}

func applyBenchmarkIntegration(t *testing.T, database *sql.DB) {
	t.Helper()
	migration, err := FS.ReadFile("019_benchmark_integration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkIntegrationBackfillsFreshMainSchema(t *testing.T) {
	database := openBenchmarkMigrationFixture(t)
	if _, err := database.Exec(`INSERT INTO runs(id,module,kind,input) VALUES('run-1','benchmarks','benchmark','{}');
INSERT INTO benchmark_results(run_id,language,requests,successes,prompt_tokens,completion_tokens,total_tokens,wall_seconds)
VALUES('run-1','go',2,1,10,5,15,3.5);`); err != nil {
		t.Fatal(err)
	}
	applyBenchmarkIntegration(t, database)
	var benchmarkID, harness string
	var tasks, passed int
	if err := database.QueryRow(`SELECT benchmark_id,harness,task_count,passed_count FROM benchmark_run_results WHERE run_id='run-1'`).Scan(&benchmarkID, &harness, &tasks, &passed); err != nil {
		t.Fatal(err)
	}
	if benchmarkID != "lmw-code-generation" || harness != "lmw-oneshot" || tasks != 1 || passed != 1 {
		t.Fatalf("backfill = %q %q %d %d", benchmarkID, harness, tasks, passed)
	}
	if _, err := database.Exec(`INSERT INTO api_tokens(id,name,token_hash,scopes_json) VALUES('token-1','factory','hash','["benchmarks:write"]')`); err != nil {
		t.Fatalf("api token schema unavailable: %v", err)
	}
}

func TestBenchmarkIntegrationPreservesExtendedRowsAndRemovesResearchForeignKey(t *testing.T) {
	database := openBenchmarkMigrationFixture(t)
	if _, err := database.Exec(`CREATE TABLE autoresearch_runs(run_id TEXT PRIMARY KEY);
INSERT INTO autoresearch_runs(run_id) VALUES('origin-1');
CREATE TABLE benchmark_run_results (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 benchmark_id TEXT NOT NULL, benchmark_version TEXT NOT NULL, harness TEXT NOT NULL,
 generation_deployment_id TEXT, verification_deployment_id TEXT,
 origin_autoresearch_run_id TEXT REFERENCES autoresearch_runs(run_id) ON DELETE SET NULL,
 execution_node_id TEXT, task_count INTEGER NOT NULL, candidate_count INTEGER NOT NULL,
 passed_count INTEGER NOT NULL DEFAULT 0, pass_at_1 REAL, verifier_pass_rate REAL,
 oracle_pass_rate REAL, prompt_tokens INTEGER NOT NULL DEFAULT 0,
 completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
 wall_seconds REAL NOT NULL DEFAULT 0, summary_artifact_id TEXT, bundle_artifact_id TEXT,
 metrics_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL
);
CREATE TABLE benchmark_trial_results (
 run_id TEXT NOT NULL REFERENCES benchmark_run_results(run_id) ON DELETE CASCADE,
 task_id TEXT NOT NULL, candidate_index INTEGER NOT NULL, official_pass INTEGER NOT NULL,
 verifier_selected INTEGER NOT NULL, verifier_score REAL, trajectory_path TEXT NOT NULL,
 metrics_json TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(run_id,task_id,candidate_index)
);
INSERT INTO runs(id,module,kind,input) VALUES('run-2','benchmarks','benchmark','{}');
INSERT INTO benchmark_run_results VALUES('run-2','terminal-bench','2','harbor',NULL,NULL,'origin-1',NULL,7,3,4,0.5,0.6,0.7,11,12,23,9.5,NULL,NULL,'{"kept":true}','2025-01-01T00:00:00Z');
INSERT INTO benchmark_trial_results VALUES('run-2','task-a',1,1,1,0.9,'trajectory.json','{"kept":true}','2025-01-01T00:00:01Z');`); err != nil {
		t.Fatal(err)
	}
	applyBenchmarkIntegration(t, database)
	var metrics string
	if err := database.QueryRow(`SELECT metrics_json FROM benchmark_run_results WHERE run_id='run-2'`).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if metrics != `{"kept":true}` {
		t.Fatalf("preserved result metrics = %q", metrics)
	}
	var legacyOrigin string
	if err := database.QueryRow(`SELECT json_extract(input,'$.origin.legacy_autoresearch_run_id') FROM runs WHERE id='run-2'`).Scan(&legacyOrigin); err != nil || legacyOrigin != "origin-1" {
		t.Fatalf("preserved legacy research origin = %q, %v", legacyOrigin, err)
	}
	var researchColumns int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('benchmark_run_results') WHERE name='origin_autoresearch_run_id'`).Scan(&researchColumns); err != nil {
		t.Fatal(err)
	}
	if researchColumns != 0 {
		t.Fatal("obsolete origin_autoresearch_run_id column remains")
	}
	var trajectory string
	if err := database.QueryRow(`SELECT trajectory_path FROM benchmark_trial_results WHERE run_id='run-2'`).Scan(&trajectory); err != nil || trajectory != "trajectory.json" {
		t.Fatalf("preserved trial = %q, %v", trajectory, err)
	}
	rows, err := database.Query(`PRAGMA foreign_key_list(benchmark_run_results)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(table, "autoresearch") {
			t.Fatalf("research foreign key remains: %s.%s", table, from)
		}
	}
}
