package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/migrate"
)

var migrationDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var migrationScanSchema = json.RawMessage(`{"type":"object","required":["legacy_root","legacy_state"],"additionalProperties":false,"properties":{"legacy_root":{"type":"string"},"legacy_state":{"type":"string"},"legacy_revision":{"type":"string","pattern":"^[0-9a-f]{40}$"},"ini_path":{"type":"string"},"docker":{"type":"boolean"}}}`)
var migrationImportSchema = json.RawMessage(`{"type":"object","required":["plan_digest","legacy_root","legacy_state","confirm"],"additionalProperties":false,"properties":{"plan_digest":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"legacy_root":{"type":"string"},"legacy_state":{"type":"string"},"legacy_revision":{"type":"string","pattern":"^[0-9a-f]{40}$"},"ini_path":{"type":"string"},"confirm":{"const":true}}}`)

func (m *Module) registerMigrationJobs(reg *jobs.Registry) {
	mustRegister := func(spec jobs.Spec) {
		if err := reg.Register("settings", spec); err != nil {
			panic(fmt.Sprintf("migration job: %v", err))
		}
	}
	mustRegister(jobs.Spec{
		Kind: "migration-scan", Title: "Scan legacy DGX Dashboard", InputSchema: migrationScanSchema,
		ArtifactKinds: []string{"file"}, LeaseResources: func(map[string]any) []string { return []string{"migration:scan"} },
		Executor: m.scanMigration,
	})
	mustRegister(jobs.Spec{
		Kind: "migration-import", Title: "Import legacy DGX Dashboard", InputSchema: migrationImportSchema,
		LeaseResources: func(input map[string]any) []string {
			digest, _ := input["plan_digest"].(string)
			return []string{"migration:" + digest}
		},
		Executor: m.importMigration,
	})
}

type migrationInput struct {
	LegacyRoot     string `json:"legacy_root"`
	LegacyState    string `json:"legacy_state"`
	LegacyRevision string `json:"legacy_revision"`
	INIPath        string `json:"ini_path"`
	Docker         bool   `json:"docker"`
	PlanDigest     string `json:"plan_digest"`
	Confirm        bool   `json:"confirm"`
}

type migrationCheckpoint struct {
	Phase  string                `json:"phase"`
	Digest string                `json:"digest"`
	Report *migrate.ImportReport `json:"report,omitempty"`
	Error  string                `json:"error,omitempty"`
}

func parseMigrationInput(input map[string]any) (migrationInput, error) {
	var out migrationInput
	data, err := json.Marshal(input)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	return out, err
}

func scanOptions(input migrationInput) (migrate.ScanOptions, error) {
	if !filepath.IsAbs(input.LegacyRoot) || !filepath.IsAbs(input.LegacyState) {
		return migrate.ScanOptions{}, fmt.Errorf("legacy_root and legacy_state must be absolute")
	}
	return migrate.ScanOptions{LegacyDir: filepath.Clean(input.LegacyRoot), StateDir: filepath.Clean(input.LegacyState), INIPath: input.INIPath, LegacyRevision: input.LegacyRevision, Docker: input.Docker}, nil
}

func (m *Module) migrationRoot() string {
	return filepath.Join(filepath.Dir(m.env.RunRoot), "migrations")
}

func (m *Module) scanMigration(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	input, err := parseMigrationInput(job.Input)
	if err != nil {
		return nil, err
	}
	opts, err := scanOptions(input)
	if err != nil {
		return nil, err
	}
	job.Logf("[migration] scanning %s", opts.LegacyDir)
	report, err := migrate.Scan(opts)
	if err != nil {
		return nil, err
	}
	plan, err := migrate.PlanJSON(&report.Plan)
	if err != nil {
		return nil, err
	}
	planName := "plan.json"
	workspacePlan := filepath.Join(job.Workspace, planName)
	if err := os.WriteFile(workspacePlan, plan, 0o600); err != nil {
		return nil, err
	}
	if _, err := job.PublishArtifact("file", planName); err != nil {
		return nil, err
	}
	stableDir := filepath.Join(m.migrationRoot(), report.Digest[7:])
	if err := os.MkdirAll(stableDir, 0o700); err != nil {
		return nil, err
	}
	stablePlan := filepath.Join(stableDir, "plan.json")
	if err := atomicWrite(stablePlan, plan); err != nil {
		return nil, err
	}
	checkpoint := migrationCheckpoint{Phase: "scanned", Digest: report.Digest}
	if err := writeCheckpoint(stableDir, checkpoint); err != nil {
		return nil, err
	}
	job.Logf("[migration] plan %s persisted", report.Digest)
	return map[string]any{"plan_digest": report.Digest, "plan_path": stablePlan, "warnings": report.Warnings}, nil
}

func (m *Module) importMigration(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	input, err := parseMigrationInput(job.Input)
	if err != nil {
		return nil, err
	}
	if !input.Confirm || !migrationDigest.MatchString(input.PlanDigest) {
		return nil, fmt.Errorf("confirmed valid plan digest is required")
	}
	scan, err := scanOptions(input)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.migrationRoot(), input.PlanDigest[7:])
	checkpoint, _ := readCheckpoint(dir)
	if checkpoint.Phase == "complete" && checkpoint.Report != nil {
		job.Logf("[migration] returning completed checkpoint %s", input.PlanDigest)
		return importOutput(checkpoint.Report), nil
	}
	planPath := filepath.Join(dir, "plan.json")
	if _, err := os.Stat(planPath); err != nil {
		return nil, fmt.Errorf("migration plan not found: %w", err)
	}
	target := filepath.Join(dir, "staging-state")
	checkpoint = migrationCheckpoint{Phase: "importing", Digest: input.PlanDigest}
	if err := writeCheckpoint(dir, checkpoint); err != nil {
		return nil, err
	}
	job.Logf("[migration] importing into isolated staging root %s", target)
	report, importErr := migrate.Import(ctx, migrate.ImportOptions{Scan: scan, PlanFile: planPath, TargetRoot: target})
	if importErr != nil {
		checkpoint.Phase, checkpoint.Report, checkpoint.Error = "failed", report, importErr.Error()
		_ = writeCheckpoint(dir, checkpoint)
		return nil, importErr
	}
	checkpoint.Phase, checkpoint.Report = "complete", report
	if err := writeCheckpoint(dir, checkpoint); err != nil {
		return nil, err
	}
	return importOutput(report), nil
}

func importOutput(report *migrate.ImportReport) map[string]any {
	return map[string]any{"plan_digest": report.PlanDigest, "staging_complete": true, "source_untouched": report.SourceUntouched, "recipes_imported": report.RecipesImported, "runs_imported": report.RunsImported, "benchmark_rows": report.BenchmarkRows, "warnings": report.Warnings}
}

func writeCheckpoint(dir string, checkpoint migrationCheckpoint) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "checkpoint.json"), data)
}

func readCheckpoint(dir string) (migrationCheckpoint, error) {
	var checkpoint migrationCheckpoint
	data, err := os.ReadFile(filepath.Join(dir, "checkpoint.json"))
	if err == nil {
		err = json.Unmarshal(data, &checkpoint)
	}
	return checkpoint, err
}

func atomicWrite(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
