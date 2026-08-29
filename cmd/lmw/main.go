// Command lmw is the operator CLI for Local Model Works: recipe catalog
// tooling, migration utilities, and state inspection.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/migrate"
)

// Version and commit are stamped at build time.
var (
	Version = "dev"
	Commit  = "none"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lmw:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "version":
		fmt.Printf("lmw %s (%s)\n", Version, Commit)
		return nil
	case "admin":
		return runAdmin(args[1:])
	case "recipe":
		return runRecipe(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return fmt.Errorf("unknown command")
	}
}

func usage() {
	fmt.Print(`lmw - Local Model Works operator CLI

Usage:
  lmw admin create --state DIR [--username NAME] [--password-stdin]
  lmw admin browser-login --state DIR [--username NAME]
  lmw recipe validate <dir>
  lmw recipe pack <dir> --output <oci-layout>
  lmw recipe init --from-git <url> --revision <ref> [--path <path>] --output <dir>
  lmw migrate dgx-dashboard scan   [--legacy DIR] [--state DIR] [--ini FILE]
                                    [--cache-root NODE=PATH]... [--out FILE]
                                    [--docker=false]
  lmw migrate dgx-dashboard import --plan FILE [--legacy DIR] [--state DIR]
                                    [--ini FILE] [--cache-root NODE=PATH]...
                                    [--node-map LEGACY=ENROLLED-NODE-ID]...
                                    [--target DIR] [--force]
  lmw help
`)
}

func runAdmin(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(`Usage:
  lmw admin create --state DIR [--username NAME] [--password-stdin]
  lmw admin browser-login --state DIR [--username NAME]

Creates the sole operator account before first start, or mints a one-use,
60-second browser login token for a local automation process.
`)
		return nil
	}
	switch args[0] {
	case "create":
		return runAdminCreate(args[1:], os.Stdin, os.Stdout)
	case "browser-login":
		return runAdminBrowserLogin(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown admin action %q (want create or browser-login)", args[0])
	}
}

func runAdminCreate(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	state := fs.String("state", "/var/lib/local-model-works", "server state root")
	username := fs.String("username", "admin", "operator username")
	passwordStdin := fs.Bool("password-stdin", false, "read one password from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("admin create: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	*state = filepath.Clean(strings.TrimSpace(*state))
	*username = strings.TrimSpace(*username)
	if *state == "" || *state == "." {
		return fmt.Errorf("admin create: --state must name a directory")
	}
	if *username == "" || len(*username) > 64 || strings.ContainsAny(*username, " \t\r\n") {
		return fmt.Errorf("admin create: --username must be 1-64 non-whitespace characters")
	}

	password, err := readAdminPassword(stdin, stdout, *passwordStdin)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("admin create: password must not be empty")
	}
	if err := os.MkdirAll(*state, 0o700); err != nil {
		return fmt.Errorf("admin create: state root: %w", err)
	}
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(*state, "lmw.db"))
	if err != nil {
		return fmt.Errorf("admin create: %w", err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)
	count, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("admin create: count users: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("admin create: an operator user already exists")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("admin create: hash password: %w", err)
	}
	if err := q.CreateUser(ctx, db.CreateUserParams{Username: *username, Argon2Hash: hash}); err != nil {
		return fmt.Errorf("admin create: create user: %w", err)
	}
	fmt.Fprintf(stdout, "operator %s created in %s\n", *username, *state)
	return nil
}

func runAdminBrowserLogin(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin browser-login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	state := fs.String("state", "/var/lib/local-model-works", "server state root")
	username := fs.String("username", "admin", "operator username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("admin browser-login: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	*state = filepath.Clean(strings.TrimSpace(*state))
	*username = strings.TrimSpace(*username)
	if *state == "" || *state == "." {
		return fmt.Errorf("admin browser-login: --state must name a directory")
	}
	if *username == "" {
		return fmt.Errorf("admin browser-login: --username is required")
	}
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(*state, "lmw.db"))
	if err != nil {
		return fmt.Errorf("admin browser-login: %w", err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)
	if _, err := q.GetUser(ctx, *username); err != nil {
		return fmt.Errorf("admin browser-login: operator %q: %w", *username, err)
	}
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	if err := q.DeleteExpiredBrowserLoginTokens(ctx, now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("admin browser-login: clean expired tokens: %w", err)
	}
	token, err := auth.NewToken()
	if err != nil {
		return fmt.Errorf("admin browser-login: %w", err)
	}
	tokenHash := fmt.Sprintf("%x", auth.SHA256([]byte(token)))
	payload, _ := json.Marshal(map[string]string{
		"username": *username, "expires_at": expires.Format(time.RFC3339Nano),
	})
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("admin browser-login: begin: %w", err)
	}
	defer tx.Rollback()
	qtx := q.WithTx(tx)
	if err := qtx.CreateBrowserLoginToken(ctx, db.CreateBrowserLoginTokenParams{
		TokenHash: tokenHash, Username: *username,
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano),
	}); err != nil {
		return fmt.Errorf("admin browser-login: persist token: %w", err)
	}
	if _, err := qtx.AppendEvent(ctx, db.AppendEventParams{
		Type: "auth.browser_login_token_created", Payload: string(payload),
	}); err != nil {
		return fmt.Errorf("admin browser-login: audit token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("admin browser-login: commit: %w", err)
	}
	return json.NewEncoder(stdout).Encode(map[string]string{
		"token": token, "expires_at": expires.Format(time.RFC3339Nano),
	})
}

func readAdminPassword(stdin io.Reader, stdout io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return "", fmt.Errorf("admin create: read password: %w", err)
		}
		password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		if strings.ContainsAny(password, "\r\n") {
			return "", fmt.Errorf("admin create: --password-stdin accepts exactly one line")
		}
		return password, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("admin create: stdin is not a terminal; use --password-stdin")
	}
	fmt.Fprint(stdout, "Password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(stdout)
	if err != nil {
		return "", fmt.Errorf("admin create: read password: %w", err)
	}
	fmt.Fprint(stdout, "Confirm password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(stdout)
	if err != nil {
		return "", fmt.Errorf("admin create: confirm password: %w", err)
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("admin create: passwords do not match")
	}
	return string(first), nil
}

func runMigrate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: lmw migrate dgx-dashboard scan|import ...")
	}
	switch args[0] {
	case "dgx-dashboard":
		if len(args) < 2 {
			return fmt.Errorf("usage: lmw migrate dgx-dashboard scan|import ...")
		}
		switch args[1] {
		case "scan":
			return runScan(args[2:])
		case "import":
			return runImport(args[2:])
		default:
			return fmt.Errorf("unknown migrate action %q (want scan or import)", args[1])
		}
	case "help", "-h", "--help":
		fmt.Print(`Usage:
  lmw migrate dgx-dashboard scan   read-only deterministic scan of the legacy
                                    DGX-Dashboard catalog and state into a
                                    digest-addressed migration plan
  lmw migrate dgx-dashboard import load a scanned plan into the new state
                                    root (the lmw-server must be STOPPED:
                                    the import writes the state root directly)
`)
		return nil
	default:
		return fmt.Errorf("unknown legacy source %q (want dgx-dashboard)", args[0])
	}
}

// scanFlags carries the repeatable --cache-root flag.
type scanFlags struct {
	cacheRoots []string
}

func (s *scanFlags) String() string { return strings.Join(s.cacheRoots, ",") }
func (s *scanFlags) Set(v string) error {
	node, path, ok := strings.Cut(v, "=")
	if !ok || node == "" || path == "" {
		return fmt.Errorf("--cache-root expects NODE=PATH, got %q", v)
	}
	s.cacheRoots = append(s.cacheRoots, v)
	return nil
}

// nodeMapFlags carries the repeatable --node-map flag.
type nodeMapFlags struct {
	mappings []string
}

func (m *nodeMapFlags) String() string { return strings.Join(m.mappings, ",") }

func (m *nodeMapFlags) Set(v string) error {
	legacyID, nodeID, ok := strings.Cut(v, "=")
	if !ok || legacyID == "" || nodeID == "" {
		return fmt.Errorf("--node-map expects LEGACY=ENROLLED-NODE-ID, got %q", v)
	}
	for _, existing := range m.mappings {
		existingLegacyID, _, _ := strings.Cut(existing, "=")
		if existingLegacyID == legacyID {
			return fmt.Errorf("--node-map repeats legacy node %q", legacyID)
		}
	}
	m.mappings = append(m.mappings, v)
	return nil
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	var opts migrate.ScanOptions
	var roots scanFlags
	var outPath string
	fs.StringVar(&opts.LegacyDir, "legacy", "", "legacy DGX-Dashboard checkout (contains control/)")
	fs.StringVar(&opts.StateDir, "state", "", "legacy state root (runs/, benchmark-results/, ...)")
	fs.StringVar(&opts.INIPath, "ini", "", "production INI (default <state>/config-production.ini)")
	fs.StringVar(&opts.LegacyRevision, "revision", "", "explicit 40-hex legacy source revision (default Git HEAD)")
	fs.Var(&roots, "cache-root", "node model/cache root binding NODE=PATH (repeatable)")
	fs.BoolVar(&opts.Docker, "docker", true, "query the docker daemon for mutable image digests")
	fs.StringVar(&outPath, "out", "", "write the full plan JSON to FILE")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, r := range roots.cacheRoots {
		node, path, _ := strings.Cut(r, "=")
		opts.CacheRoots = append(opts.CacheRoots, migrate.CacheRootSpec{Node: node, Path: path})
	}
	if opts.LegacyDir == "" || opts.StateDir == "" {
		return fmt.Errorf("--legacy and --state are required")
	}
	report, err := migrate.Scan(opts)
	if err != nil {
		return err
	}
	printScanSummary(report, opts)
	if outPath != "" {
		b, err := migrate.PlanJSON(&report.Plan)
		if err != nil {
			return err
		}
		if err := os.WriteFile(outPath, append(b, '\n'), 0o644); err != nil {
			return fmt.Errorf("write plan: %w", err)
		}
		fmt.Fprintf(os.Stderr, "plan written to %s\n", outPath)
	}
	return nil
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "import loads the plan into a fresh (or stopped-server) state root.")
		fmt.Fprintln(fs.Output(), "Stop lmw-server before running import: it writes the state root directly.")
		fmt.Fprintln(fs.Output())
		fmt.Fprintln(fs.Output(), "Usage of import:")
		fs.PrintDefaults()
	}
	var opts migrate.ImportOptions
	var roots scanFlags
	var nodeMaps nodeMapFlags
	var planPath, target string
	fs.StringVar(&planPath, "plan", "", "migration plan JSON produced by scan --out (required)")
	fs.StringVar(&opts.Scan.LegacyDir, "legacy", "", "legacy DGX-Dashboard checkout (contains control/)")
	fs.StringVar(&opts.Scan.StateDir, "state", "", "legacy state root (runs/, benchmark-results/, ...)")
	fs.StringVar(&opts.Scan.INIPath, "ini", "", "production INI (default <state>/config-production.ini)")
	fs.StringVar(&opts.Scan.LegacyRevision, "revision", "", "explicit 40-hex legacy source revision (default Git HEAD)")
	fs.Var(&roots, "cache-root", "node model/cache root binding NODE=PATH (repeatable)")
	fs.Var(&nodeMaps, "node-map", "legacy node=enrolled node ID binding (repeatable)")
	fs.BoolVar(&opts.Scan.Docker, "docker", true, "query the docker daemon for mutable image digests")
	fs.StringVar(&target, "target", os.Getenv("LMW_STATE_ROOT"), "new state root (default $LMW_STATE_ROOT or /var/lib/local-model-works)")
	fs.BoolVar(&opts.Force, "force", false, "override digest mismatch and nonterminal-run aborts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, r := range roots.cacheRoots {
		node, path, _ := strings.Cut(r, "=")
		opts.Scan.CacheRoots = append(opts.Scan.CacheRoots, migrate.CacheRootSpec{Node: node, Path: path})
	}
	if len(nodeMaps.mappings) > 0 {
		opts.NodeMap = make(map[string]string, len(nodeMaps.mappings))
		for _, mapping := range nodeMaps.mappings {
			legacyID, nodeID, _ := strings.Cut(mapping, "=")
			opts.NodeMap[legacyID] = nodeID
		}
	}
	if target == "" {
		target = "/var/lib/local-model-works"
	}
	opts.PlanFile = planPath
	opts.TargetRoot = target
	rep, err := migrate.Import(context.Background(), opts)
	if err != nil {
		if rep != nil && len(rep.NonterminalAborted) > 0 {
			printImportReport(rep)
		}
		return err
	}
	printImportReport(rep)
	return nil
}

func printScanSummary(r *migrate.Report, opts migrate.ScanOptions) {
	c := r.Plan.Counts
	fmt.Printf("dgx-dashboard migration scan\n")
	fmt.Printf("legacy:   %s\n", opts.LegacyDir)
	fmt.Printf("state:    %s\n", opts.StateDir)
	fmt.Printf("recipes:  %d single-node packages -> %d recipes (%d merged multi-target)\n",
		c.SingleNodePackages, c.SingleNodeRecipes, c.MergedRecipes)
	fmt.Printf("cluster:  %d packages (hand conversion drafts)\n", c.ClusterPackages)
	fmt.Printf("runs:     %d terminal, %d nonterminal\n", c.RunsTerminal, c.RunsNonterminal)
	if ids := migrate.NonterminalIDs(r.Plan.Runs); len(ids) > 0 {
		fmt.Printf("nonterminal: %s\n", strings.Join(ids, ", "))
	}
	fmt.Printf("benchmarks: %d index entries, %d result files, %d aider files\n",
		c.BenchmarkIndexEntries, c.BenchmarkResultsFiles, c.AiderBenchmarkFiles)
	fmt.Printf("placements: %d verified, %d failed\n",
		c.Placements-c.PlacementFailures, c.PlacementFailures)
	if len(r.Plan.Strays) > 0 {
		fmt.Printf("strays (%d, not imported):\n", len(r.Plan.Strays))
		for _, s := range r.Plan.Strays {
			fmt.Printf("  %s: %s\n", s.Path, s.Reason)
		}
	}
	if c.MutableImages > 0 {
		fmt.Printf("mutable images (%d, not launchable until published by digest):\n", c.MutableImages)
		seen := map[string]bool{}
		for _, rec := range r.Plan.Recipes {
			if rec.Image.Mutable && !seen[rec.Image.Reference] {
				seen[rec.Image.Reference] = true
				fmt.Printf("  %s\n", rec.Image.Reference)
			}
		}
		for _, d := range r.Plan.ClusterDrafts {
			if d.Image.Mutable && !seen[d.Image.Reference] {
				seen[d.Image.Reference] = true
				fmt.Printf("  %s\n", d.Image.Reference)
			}
		}
	}
	if len(r.Containers) > 0 {
		fmt.Printf("current legacy containers (%d, cutover intent, not plan input):\n", len(r.Containers))
		for _, ct := range r.Containers {
			fmt.Printf("  %s [%s] %s\n", ct.Name, ct.Image, ct.Status)
		}
	}
	if len(r.Warnings) > 0 {
		for _, w := range r.Warnings {
			fmt.Printf("warning: %s\n", w)
		}
	}
	fmt.Printf("plan digest: %s\n", r.Digest)
}

func printImportReport(r *migrate.ImportReport) {
	fmt.Printf("dgx-dashboard migration import\n")
	fmt.Printf("plan digest:    %s\n", r.PlanDigest)
	fmt.Printf("rescan digest:  %s\n", r.RescanDigest)
	fmt.Printf("recipes:        %d imported, %d already present\n", r.RecipesImported, r.RecipesExisting)
	fmt.Printf("runs:           %d imported, %d already present, %d ghost rows\n",
		r.RunsImported, r.RunsExisting, r.GhostRunsCreated)
	fmt.Printf("logs:           %d\n", r.LogsImported)
	fmt.Printf("benchmark files: %d hardlinked, %d copied\n", r.BenchmarkFilesLinked, r.BenchmarkFilesCopied)
	fmt.Printf("benchmark rows:  %d\n", r.BenchmarkRows)
	fmt.Printf("placements:      %d registered, %d failed validation\n",
		r.PlacementsRegistered, len(r.PlacementFailures))
	for _, f := range r.PlacementFailures {
		fmt.Printf("  placement failure: %s\n", f)
	}
	for _, w := range r.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	if r.SourceUntouched {
		fmt.Printf("source trees:   unchanged (%s)\n", r.SourceDigestBefore)
	} else {
		fmt.Printf("source trees:   CHANGED (before %s, after %s)\n",
			r.SourceDigestBefore, r.SourceDigestAfter)
	}
}
