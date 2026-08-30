package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/hf"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
)

// importFiles copies the two benchmark result trees (hardlink on a shared
// filesystem, otherwise copy + SHA-256 verify) and each terminal run's
// output.log into the new run root. Copies happen before the DB transaction
// so a rolled-back import never references missing files.
func importFiles(ctx context.Context, plan *Plan, opts ImportOptions, runsSvc *runs.Service, rep *ImportReport) error {
	if err := copyTree(ctx, plan.Benchmarks.ResultsDir,
		filepath.Join(opts.TargetRoot, "benchmarks", "results"), rep); err != nil {
		return fmt.Errorf("benchmark-results: %w", err)
	}
	if err := copyTree(ctx, plan.Benchmarks.AiderDir,
		filepath.Join(opts.TargetRoot, "benchmarks", "aider-benchmarks"), rep); err != nil {
		return fmt.Errorf("aider-benchmarks: %w", err)
	}
	for _, r := range plan.Runs {
		if r.Nonterminal || r.LogSize == 0 {
			continue
		}
		src := filepath.Join(opts.Scan.StateDir, "runs", r.ID, "output.log")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := runsSvc.LogPath(r.ID, "", 0, "stdout")
		if dst == "" {
			return fmt.Errorf("run log path: unsafe id %s", r.ID)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if _, err := copyFileVerified(src, dst); err != nil {
			return fmt.Errorf("run log %s: %w", r.ID, err)
		}
		rep.LogsImported++
	}
	return nil
}

func copyTree(ctx context.Context, srcRoot, dstRoot string, rep *ImportReport) error {
	st, err := os.Stat(srcRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", srcRoot)
	}
	return filepath.WalkDir(srcRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(srcRoot, p)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(dstRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		linked, cerr := copyFileVerified(p, dst)
		if cerr != nil {
			return cerr
		}
		if linked {
			rep.BenchmarkFilesLinked++
		} else {
			rep.BenchmarkFilesCopied++
		}
		return nil
	})
}

// copyFileVerified hardlinks src onto dst when possible (same filesystem;
// the source file is never modified), otherwise copies and SHA-256 verifies.
func copyFileVerified(src, dst string) (bool, error) {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.Link(src, dst); err == nil {
		return true, nil
	}
	if err := copyFileSHA(src, dst); err != nil {
		return false, err
	}
	return false, nil
}

func copyFileSHA(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	sumA, _ := fileSHA256(src)
	sumB, _ := fileSHA256(dst)
	if sumA == "" || sumB == "" {
		return fmt.Errorf("verification failed: unreadable file")
	}
	if sumA != sumB {
		return fmt.Errorf("verification mismatch after copy: %s", dst)
	}
	return nil
}

// importDB performs the single transactional DB mutation: plan record, node
// seeds, recipes, terminal runs (legacy state map + original timestamps),
// benchmark result rows, and validated cache placements.
func importDB(ctx context.Context, sqlDB *sql.DB, validator *recipe.Validator,
	plan *Plan, rep *ImportReport, opts ImportOptions) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)

	if _, err := q.GetMigrationPlan(ctx, plan.Digest); errors.Is(err, sql.ErrNoRows) {
		if err := q.SaveMigrationPlan(ctx, db.SaveMigrationPlanParams{
			PlanDigest: plan.Digest,
			Request:    "dgx-dashboard migration",
			Plan:       canonicalPlanJSON(plan),
		}); err != nil {
			return fmt.Errorf("save migration plan: %w", err)
		}
	}

	for _, n := range plan.Nodes {
		if _, mapped := opts.NodeMap[n.ID]; mapped {
			continue
		}
		if _, err := q.GetNode(ctx, n.ID); errors.Is(err, sql.ErrNoRows) {
			labels, _ := json.Marshal(map[string]string{"source": "dgx-dashboard-migration"})
			if err := q.CreateNode(ctx, db.CreateNodeParams{
				ID:          n.ID,
				DisplayName: n.ID,
				Labels:      string(labels),
				CreatedAt:   nowStamp(),
			}); err != nil {
				return fmt.Errorf("seed node %s: %w", n.ID, err)
			}
		} else if err != nil {
			return err
		}
	}

	var createdRecipes []string
	for _, r := range plan.Recipes {
		if _, err := q.GetRecipe(ctx, r.Digest); err == nil {
			rep.RecipesExisting++
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, vdiags, err := validator.ValidateStrict(r.Document); err != nil {
			return fmt.Errorf("import recipe %s: %v (%v)", r.Name, err, vdiags)
		} else if bad := launchBlockers(vdiags); len(bad) > 0 {
			return fmt.Errorf("import recipe %s: %v", r.Name, bad)
		} else if r.Image.Mutable {
			// Mutable images are stored but are not launchable until published
			// by digest; the strict validator finding is expected and recorded.
			rep.Warnings = append(rep.Warnings, fmt.Sprintf(
				"recipe %s: stored with mutable image %s; not launchable until published by digest",
				r.Name, r.Image.Reference))
		}
		if err := q.CreateRecipe(ctx, db.CreateRecipeParams{
			Digest:   r.Digest,
			Name:     r.Name,
			Version:  "1.0.0",
			Source:   r.Packages[0],
			Manifest: string(r.Document),
		}); err != nil {
			return fmt.Errorf("create recipe %s: %w", r.Name, err)
		}
		createdRecipes = append(createdRecipes, r.Name+"@"+r.Digest)
		rep.RecipesImported++
	}

	importedRuns := map[string]bool{}
	for _, r := range plan.Runs {
		if r.Nonterminal {
			if opts.Force {
				rep.Warnings = append(rep.Warnings, "nonterminal legacy run skipped with --force: "+r.ID)
			}
			continue
		}
		if _, err := q.GetRun(ctx, r.ID); err == nil {
			importedRuns[r.ID] = true
			rep.RunsExisting++
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		resources, _ := json.Marshal(r.Resources)
		identity := sql.NullString{}
		if r.LegacyIdentity != "" {
			identity = sql.NullString{String: r.LegacyIdentity, Valid: true}
		}
		if err := q.CreateRun(ctx, db.CreateRunParams{
			ID:             r.ID,
			Module:         LegacyModule,
			Kind:           r.Kind,
			State:          string(runs.Queued),
			Resources:      string(resources),
			Input:          string(r.Request),
			LegacyIdentity: identity,
		}); err != nil {
			return fmt.Errorf("create run %s: %w", r.ID, err)
		}
		if err := transitionRun(ctx, q, r.ID, r.State); err != nil {
			return err
		}
		if err := q.UpdateRunState(ctx, db.UpdateRunStateParams{
			State:        r.State,
			StartedAt:    nullStr(r.StartedAt),
			FinishedAt:   nullStr(r.FinishedAt),
			ErrorCode:    nullStr(r.ErrorCode),
			ErrorMessage: nullStr(legacyStateMessage(r)),
			ID:           r.ID,
		}); err != nil {
			return fmt.Errorf("finalize run %s: %w", r.ID, err)
		}
		importedRuns[r.ID] = true
		rep.RunsImported++
	}

	rows, ghosts, err := benchmarkRows(plan, importedRuns, opts.Scan.StateDir, opts.TargetRoot)
	if err != nil {
		return err
	}
	for _, id := range ghosts {
		if importedRuns[id] {
			continue
		}
		if err := q.CreateRun(ctx, db.CreateRunParams{
			ID:             id,
			Module:         LegacyModule,
			Kind:           "benchmark",
			State:          string(runs.Queued),
			Resources:      "[]",
			Input:          "{}",
			LegacyIdentity: sql.NullString{String: id, Valid: true},
		}); err != nil {
			return fmt.Errorf("create ghost run %s: %w", id, err)
		}
		if err := transitionRun(ctx, q, id, string(runs.Succeeded)); err != nil {
			return err
		}
		importedRuns[id] = true
		rep.GhostRunsCreated++
	}
	for _, row := range rows {
		existing, err := q.ListBenchmarkResultsByRun(ctx, row.RunID)
		if err != nil {
			return err
		}
		skip := false
		for _, e := range existing {
			if e.Language == row.Language {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if err := q.InsertBenchmarkResult(ctx, row); err != nil {
			return fmt.Errorf("insert benchmark row %s/%s: %w", row.RunID, row.Language, err)
		}
		rep.BenchmarkRows++
	}

	verifiedNow := nowStamp()
	for _, n := range plan.Nodes {
		nodeID := opts.mappedNodeID(n.ID)
		for _, cr := range n.CacheRoots {
			for _, pl := range cr.Placements {
				if err := importPlacement(ctx, q, nodeID, pl, verifiedNow, rep); err != nil {
					return err
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	bus := events.NewEventBus(db.New(sqlDB))
	for _, name := range createdRecipes {
		payload, _ := json.Marshal(map[string]string{"name": name})
		if err := bus.Publish(ctx, "recipe.imported", "migration", payload); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("event for recipe %s: %v", name, err))
		}
	}
	return nil
}

func importPlacement(ctx context.Context, q *db.Queries, nodeID string, pl Placement, verifiedNow string, rep *ImportReport) error {
	ident := pl.Identity
	id := artifactID(ident)
	if _, err := q.GetArtifact(ctx, id); errors.Is(err, sql.ErrNoRows) {
		meta, _ := json.Marshal(map[string]string{
			"source": "dgx-dashboard-migration",
			"node":   nodeID,
			"path":   pl.Path,
		})
		if err := q.CreateArtifact(ctx, db.CreateArtifactParams{
			ID:       id,
			Kind:     "model",
			Identity: ident,
			Revision: nullStr(pl.Revision),
			Metadata: string(meta),
		}); err != nil {
			return fmt.Errorf("create artifact %s: %w", id, err)
		}
	} else if err != nil {
		return err
	}
	// Re-validate at import time: a placement whose snapshot no longer
	// validates is a reported error, not a silent skip.
	state := pl.State
	diagnostics := pl.Diagnostics
	if strings.HasPrefix(pl.Identity, "hf://") {
		diags := hf.ValidateSnapshot(pl.Path, hfHubDir(pl.Path))
		if len(diags) > 0 {
			state = "failed"
			diagnostics = nil
			for _, d := range diags {
				diagnostics = append(diagnostics, d.Code+": "+d.Message)
			}
			rep.PlacementFailures = append(rep.PlacementFailures,
				fmt.Sprintf("%s on %s: %s", pl.Identity, nodeID, strings.Join(diagnostics, "; ")))
		} else {
			state = "verified"
		}
	}
	diagJSON, _ := json.Marshal(diagnostics)
	verifiedAt := sql.NullString{}
	if state == "verified" {
		verifiedAt = sql.NullString{String: verifiedNow, Valid: true}
	}
	if err := q.UpsertPlacement(ctx, db.UpsertPlacementParams{
		ArtifactID:  id,
		NodeID:      nodeID,
		Path:        pl.Path,
		State:       state,
		VerifiedAt:  verifiedAt,
		Diagnostics: string(diagJSON),
		SizeBytes:   pl.SizeBytes,
	}); err != nil {
		return fmt.Errorf("upsert placement %s: %w", ident, err)
	}
	rep.PlacementsRegistered++
	return nil
}

// artifactID derives the stable dgx-<hex16> identity for one placement
// identity (identity + pinned revision).
func artifactID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "dgx-" + hex.EncodeToString(sum[:])[:16]
}

// hfHubDir returns the largest HF directory a snapshot symlink may escape
// to: the models--<org>--<repo> directory.
func hfHubDir(snapPath string) string {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(snapPath)), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.HasPrefix(parts[i], "models--") {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
}

func nowStamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// legacyStateMessage preserves the original legacy state on failed imports
// (the plan keeps it in metadata; the error message names it too).
func legacyStateMessage(r RunEntry) string {
	if r.State != "failed" || r.LegacyState == "" {
		return ""
	}
	return "legacy state " + r.LegacyState
}

// launchBlockers returns the validation findings that make a converted
// recipe unimportable. recipe.image-latest is tolerated: mutable-image
// recipes are stored verbatim and become launchable once the operator pins
// them by digest (scan reports them separately).
func launchBlockers(diags []recipe.Diagnostic) []recipe.Diagnostic {
	var out []recipe.Diagnostic
	for _, d := range diags {
		if d.Code != "recipe.image-latest" {
			out = append(out, d)
		}
	}
	return out
}

// transitionRun drives the product state graph from Queued to the legacy
// target state with the exact allowed steps:
//
//	succeeded   queued -> running -> verifying -> succeeded
//	failed      queued -> failed
//	cancelled   queued -> cancelling -> cancelled
//	interrupted queued -> running -> interrupted
func transitionRun(ctx context.Context, q *db.Queries, id, target string) error {
	steps := map[string][]string{
		"succeeded":   {"running", "verifying", "succeeded"},
		"cancelled":   {"cancelling", "cancelled"},
		"interrupted": {"running", "interrupted"},
		"failed":      {"failed"},
	}
	seq, ok := steps[target]
	if !ok {
		return fmt.Errorf("no transition path to legacy target state %q", target)
	}
	for _, st := range seq {
		if err := q.UpdateRunState(ctx, db.UpdateRunStateParams{State: st, ID: id}); err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	return nil
}

// canonicalPlanJSON renders the plan document for the migration_plans row.
func canonicalPlanJSON(p *Plan) string {
	b, err := cjson.Marshal(p)
	if err != nil {
		panic(err) // plan marshal failures are logic errors
	}
	return string(b)
}
