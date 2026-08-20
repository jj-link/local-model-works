package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Scan performs the full deterministic scan of the legacy tree and state
// and returns the operator report (plan + digest + report-only facts).
func Scan(opts ScanOptions) (*Report, error) {
	if opts.LegacyMissing() {
		return nil, fmt.Errorf("--legacy is required")
	}
	if opts.StateDir == "" {
		return nil, fmt.Errorf("--state is required")
	}
	if opts.INIPath == "" {
		opts.INIPath = defaultINIPath(opts.StateDir)
	}

	resolver := newImageResolver(opts.Docker)

	// 1. Catalog: single-node packages, cluster packages, strays.
	catalog, err := ScanCatalog(opts.LegacyDir)
	if err != nil {
		return nil, err
	}

	// 2. Recipes from single-node packages (dedup by exact contract).
	recipes := ConvertRecipes(catalog, resolver, legacyRevisionOf(opts.LegacyDir), opts.LegacyDir)

	// 3. Cluster drafts.
	drafts := ConvertClusterDrafts(catalog, resolver)

	// 4. Runs: request → recipe digest join. The key uses the original
	// legacy package name (merged recipes share it across target copies),
	// so a legacy request for (engine, artifact, target) resolves to the
	// converted recipe that covers that target.
	digests := map[string]string{}
	for _, r := range recipes {
		legacyName := legacyPackageName(r)
		for _, t := range r.Targets {
			digests[r.Engine+"/"+legacyName+"/"+t] = r.Digest
		}
	}
	runs, err := ScanRuns(opts.StateDir, digests)
	if err != nil {
		return nil, err
	}

	// 5. Benchmarks.
	bench, err := ScanBenchmarks(opts.StateDir)
	if err != nil {
		return nil, err
	}

	// 6. Cache roots and placements.
	nodes := ScanCacheRoots(opts)

	plan := Plan{
		Schema:        PlanSchema,
		Nodes:         nodes,
		Recipes:       recipes,
		ClusterDrafts: drafts,
		Runs:          runs,
		Benchmarks:    bench,
		Strays:        catalog.Strays,
	}
	fillCounts(&plan, catalog, recipes, runs)
	SortPlan(&plan)
	plan.Digest = PlanDigestOf(&plan)

	rep := &Report{Plan: plan, Digest: plan.Digest}
	rep.Containers = runningLegacyContainers()
	if opts.Docker {
		for _, r := range plan.Recipes {
			if r.Image.Mutable {
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("recipe %s: mutable image %s is not launchable until published by digest", r.Name, r.Image.Reference))
			}
		}
		sort.Strings(rep.Warnings)
	} else {
		for _, r := range plan.Recipes {
			if r.Image.Mutable {
				rep.Warnings = append(rep.Warnings,
					fmt.Sprintf("recipe %s: mutable image %s (digest derived; docker not consulted)", r.Name, r.Image.Reference))
			}
		}
		sort.Strings(rep.Warnings)
	}
	return rep, nil
}

func (o ScanOptions) LegacyMissing() bool { return o.LegacyDir == "" }

// legacyRevisionOf reads the legacy repository's current HEAD commit (40-hex)
// for provenance; empty when the tree is not a git checkout.
func legacyRevisionOf(legacyDir string) string {
	head, err := os.ReadFile(filepath.Join(legacyDir, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	if ref, ok := strings.CutPrefix(line, "ref: "); ok {
		raw, err := os.ReadFile(filepath.Join(legacyDir, ".git", strings.TrimSpace(ref)))
		if err != nil {
			return ""
		}
		line = strings.TrimSpace(string(raw))
	}
	if len(line) != 40 {
		return ""
	}
	return line
}

func defaultINIPath(stateDir string) string {
	return stateDir + "/config-production.ini"
}

// legacyPackageName recovers the original legacy package name (underscores)
// from a converted recipe entry.
func legacyPackageName(r RecipeEntry) string {
	// Packages carry the relative path; the final segment is the legacy name.
	return lastPathSegment(r.Packages[0])
}

func lastPathSegment(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// fillCounts tallies every plan category.
func fillCounts(p *Plan, catalog *CatalogScan, recipes []RecipeEntry, runs []RunEntry) {
	c := &p.Counts
	c.SingleNodePackages = len(catalog.Single)
	c.SingleNodeRecipes = len(recipes)
	for _, r := range recipes {
		if r.Merged {
			c.MergedRecipes++
		}
	}
	c.ClusterPackages = len(catalog.Cluster)
	c.Strays = len(p.Strays)
	c.MutableImages = 0
	seen := map[string]bool{}
	for _, r := range recipes {
		if r.Image.Mutable && !seen[r.Image.Reference] {
			seen[r.Image.Reference] = true
			c.MutableImages++
		}
	}
	for _, d := range p.ClusterDrafts {
		if d.Image.Mutable && !seen[d.Image.Reference] {
			seen[d.Image.Reference] = true
			c.MutableImages++
		}
	}
	c.RunsTerminal = 0
	c.RunsNonterminal = 0
	for _, r := range runs {
		if r.Nonterminal {
			c.RunsNonterminal++
		} else {
			c.RunsTerminal++
		}
	}
	c.BenchmarkIndexEntries = p.Benchmarks.IndexEntries
	c.BenchmarkResultsFiles = p.Benchmarks.ResultsFiles
	c.AiderBenchmarkFiles = p.Benchmarks.AiderFiles
	c.Placements = 0
	c.PlacementFailures = 0
	for _, n := range p.Nodes {
		for _, cr := range n.CacheRoots {
			for _, pl := range cr.Placements {
				c.Placements++
				if pl.State == "failed" {
					c.PlacementFailures++
				}
			}
		}
	}
}

// runningLegacyContainers lists currently running legacy serving containers
// (report-only: container state is cutover intent, never plan input).
func runningLegacyContainers() []LegacyContainer {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}|{{.Image}}|{{.Status}}").Output()
	if err != nil {
		return nil
	}
	var cs []LegacyContainer
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		if !strings.HasPrefix(parts[0], "inference-") {
			continue
		}
		cs = append(cs, LegacyContainer{Name: parts[0], Image: parts[1], Status: parts[2]})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs
}

// PlanJSON renders the plan document for --out.
func PlanJSON(p *Plan) ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}
