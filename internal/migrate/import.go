package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
)

// ImportOptions are the inputs for one import: the same scan inputs used to
// produce the plan, the plan file, and the target (new) state root.
type ImportOptions struct {
	Scan       ScanOptions
	PlanFile   string
	TargetRoot string
	Force      bool
	// NodeMap maps legacy plan node IDs to already-enrolled node IDs.
	// Unmapped plan nodes retain the standalone-import behavior and are seeded.
	NodeMap map[string]string
}

// ImportReport is the operator-facing import result.
type ImportReport struct {
	PlanDigest           string
	RescanDigest         string
	RecipesImported      int
	RecipesExisting      int
	RunsImported         int
	RunsExisting         int
	NonterminalAborted   []string
	GhostRunsCreated     int
	LogsImported         int
	BenchmarkFilesLinked int
	BenchmarkFilesCopied int
	BenchmarkRows        int
	PlacementsRegistered int
	PlacementFailures    []string
	SourceUntouched      bool
	SourceDigestBefore   string
	SourceDigestAfter    string
	Warnings             []string
}

func (opts ImportOptions) mappedNodeID(legacyID string) string {
	if nodeID, ok := opts.NodeMap[legacyID]; ok {
		return nodeID
	}
	return legacyID
}

func validateNodeMap(plan *Plan, nodeMap map[string]string) error {
	if len(nodeMap) == 0 {
		return nil
	}
	planNodes := make(map[string]struct{}, len(plan.Nodes))
	for _, node := range plan.Nodes {
		planNodes[node.ID] = struct{}{}
	}
	for legacyID, nodeID := range nodeMap {
		if nodeID == "" {
			return fmt.Errorf("node map %q has an empty destination", legacyID)
		}
		if _, ok := planNodes[legacyID]; !ok {
			return fmt.Errorf("node map %q is not a node in the migration plan", legacyID)
		}
	}
	return nil
}

func validateMappedNodes(ctx context.Context, q *db.Queries, plan *Plan, opts ImportOptions) error {
	for _, legacyNode := range plan.Nodes {
		nodeID, mapped := opts.NodeMap[legacyNode.ID]
		if !mapped {
			continue
		}
		if _, err := q.GetNode(ctx, nodeID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("node map %s=%s: enrolled node not found", legacyNode.ID, nodeID)
			}
			return fmt.Errorf("node map %s=%s: %w", legacyNode.ID, nodeID, err)
		}
	}
	return nil
}

// Import loads the plan into the target state root. The server must be
// stopped: the import writes the state root directly. Sequence:
//
//  1. re-scan the legacy inputs; the fresh digest must equal the plan's
//     (else error with both, --force overrides);
//  2. snapshot the source trees (pre-manifest);
//  3. refuse to start while a nonterminal legacy run exists (--force
//     skips importing it);
//  4. copy benchmark result trees (hardlink on one filesystem, else copy +
//     SHA-256 verify) and run logs into the new run root;
//  5. one DB transaction: plan record, node seeds, recipes (trust_local),
//     terminal runs with the legacy state map and original timestamps,
//     benchmark result rows, and validated cache placements;
//  6. verify: logs readable through the runs service with recorded end
//     offset, benchmark counts reproduced, placements re-validated, and the
//     post-manifest equal to the pre-manifest.
func Import(ctx context.Context, opts ImportOptions) (*ImportReport, error) {
	rep := &ImportReport{}
	if opts.PlanFile == "" {
		return rep, fmt.Errorf("--plan is required")
	}
	if opts.TargetRoot == "" {
		return rep, fmt.Errorf("target state root is required (--target or %s)", "LMW_STATE_ROOT")
	}
	if opts.Scan.INIPath == "" {
		opts.Scan.INIPath = defaultINIPath(opts.Scan.StateDir)
	}

	plan, err := loadPlanFile(opts.PlanFile)
	if err != nil {
		return rep, err
	}
	rep.PlanDigest = plan.Digest
	if err := validateNodeMap(plan, opts.NodeMap); err != nil {
		return rep, err
	}

	rescanned, err := Scan(opts.Scan)
	if err != nil {
		return rep, fmt.Errorf("re-scan: %w", err)
	}
	rep.RescanDigest = rescanned.Digest
	if rescanned.Digest != plan.Digest {
		if !opts.Force {
			return rep, fmt.Errorf(
				"plan digest mismatch: plan %s, fresh scan %s (use --force to override)",
				plan.Digest, rescanned.Digest)
		}
		rep.Warnings = append(rep.Warnings,
			fmt.Sprintf("plan digest mismatch overridden with --force: plan %s, fresh scan %s",
				plan.Digest, rescanned.Digest))
	}

	nonterminal := NonterminalIDs(rescanned.Plan.Runs)
	if len(nonterminal) > 0 && !opts.Force {
		rep.NonterminalAborted = nonterminal
		return rep, fmt.Errorf(
			"import aborted: %d nonterminal legacy run(s) still owned by the old manager: %s; stop them or use --force",
			len(nonterminal), strings.Join(nonterminal, ", "))
	}

	before, err := ManifestTree(ScanInputs(opts.Scan))
	if err != nil {
		return rep, fmt.Errorf("source manifest: %w", err)
	}
	rep.SourceDigestBefore = before.Digest

	sqlDB, err := db.Open(ctx, filepath.Join(opts.TargetRoot, "lmw.db"))
	if err != nil {
		return rep, fmt.Errorf("open target db: %w", err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)
	bus := events.NewEventBus(q)
	if err := validateMappedNodes(ctx, q, plan, opts); err != nil {
		return rep, err
	}
	validator, err := recipe.NewValidator()
	if err != nil {
		return rep, fmt.Errorf("recipe validator: %w", err)
	}
	runsSvc := runs.New(sqlDB, q, bus, filepath.Join(opts.TargetRoot, "runs"))

	if err := importFiles(ctx, plan, opts, runsSvc, rep); err != nil {
		return rep, err
	}
	if err := importDB(ctx, sqlDB, validator, plan, rep, opts); err != nil {
		return rep, err
	}
	if err := verifyImport(ctx, plan, opts, runsSvc, rep); err != nil {
		return rep, err
	}

	after, err := ManifestTree(ScanInputs(opts.Scan))
	if err != nil {
		return rep, fmt.Errorf("post source manifest: %w", err)
	}
	rep.SourceDigestAfter = after.Digest
	rep.SourceUntouched = before.Digest == after.Digest
	if !rep.SourceUntouched {
		return rep, fmt.Errorf("source trees changed during import: before %s, after %s",
			before.Digest, after.Digest)
	}
	return rep, nil
}

func loadPlanFile(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	if p.Schema != PlanSchema {
		return nil, fmt.Errorf("unknown plan schema %q", p.Schema)
	}
	if err := ValidatePlanDigest(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
