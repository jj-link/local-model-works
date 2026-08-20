package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// legacyTargets mirrors catalog.py _TARGETS; the scanner enumerates all of
// them so the plan is complete regardless of the INI targets list.
var legacyTargets = []string{"local", "spark1", "spark2", "spark3", "cluster"}

var legacyEngines = []string{"vllm", "sglang"}

// profileArch maps the single-node serve profiles to accelerator
// architectures: rtx6000 is RTX PRO 6000 Blackwell (SM120), spark is the
// DGX Spark GB10 (SM121). The legacy build scripts pin these lists.
var profileArch = map[string]string{"rtx6000": "sm_120", "spark": "sm_121"}

// profileTargets maps each single-node profile to the legacy targets it
// serves (catalog.py single_profiles).
var profileTargets = map[string][]string{
	"rtx6000": {"local"},
	"spark":   {"spark1", "spark2", "spark3"},
}

var artifactRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// SinglePackage is one catalog-discovered single-node serve package.
type SinglePackage struct {
	Engine   string
	Profile  string
	Name     string
	RelPath  string // relative to the legacy root, e.g. control/serve/vllm/rtx6000/x
	AbsPath  string
	Meta     RuntimeMeta
	ServeSH  []byte
	ServeSHA string
	CapsSHA  string // "" when no capabilities.json
	Assets   []FileRef
	Targets  []string
	Arch     string
}

// ClusterPackage is one catalog-discovered cluster serve package.
type ClusterPackage struct {
	Engine      string
	Name        string
	RelPath     string
	AbsPath     string
	Meta        RuntimeMeta
	DefaultProf string
	Profiles    []ClusterProfile
	Files       []FileRef
	HeadHost    string
	WorkerHost  string
	APIPort     int
}

// CatalogScan is the catalog enumeration result.
type CatalogScan struct {
	Single  []SinglePackage
	Cluster []ClusterPackage
	Strays  []Stray
}

// ScanCatalog enumerates control/serve exactly as catalog.py does and
// reports every non-catalog tree as a stray. Read-only.
func ScanCatalog(legacyDir string) (*CatalogScan, error) {
	controlRoot := filepath.Join(legacyDir, "control")
	parser := filepath.Join(controlRoot, "tools", "parse-runtime-env.py")
	if st, err := os.Stat(parser); err != nil || (st.Mode().Perm()&0o111) == 0 {
		return nil, fmt.Errorf("the runtime metadata parser is unavailable: %s", parser)
	}
	serveRoot := filepath.Join(controlRoot, "serve")
	st, err := os.Stat(serveRoot)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("serve root unavailable: %w", err)
	}

	out := &CatalogScan{}
	entries, err := os.ReadDir(serveRoot)
	if err != nil {
		return nil, fmt.Errorf("read serve root: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		rel := filepath.Join("control", "serve", name)
		switch {
		case name == "vllm" || name == "sglang":
			scanEngine(serveRoot, name, out)
		case name == "cluster":
			for _, engine := range legacyEngines {
				scanClusterEngine(engine, serveRoot, out)
			}
		default:
			out.Strays = append(out.Strays, Stray{
				Path:   rel,
				Reason: "not a catalog engine directory",
			})
		}
	}

	// cluster engine bases live under control/serve/cluster/<engine>;
	// anything else directly under cluster/ is a stray.
	clusterRoot := filepath.Join(serveRoot, "cluster")
	if st, err := os.Stat(clusterRoot); err == nil && st.IsDir() {
		centries, err := os.ReadDir(clusterRoot)
		if err != nil {
			return nil, fmt.Errorf("read cluster root: %w", err)
		}
		for _, e := range centries {
			name := e.Name()
			rel := filepath.Join("control", "serve", "cluster", name)
			if name == "vllm" || name == "sglang" {
				continue
			}
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "not a catalog cluster engine directory"})
		}
	} else {
		out.Strays = append(out.Strays, Stray{Path: "control/serve/cluster", Reason: "missing cluster serve root"})
	}
	sort.Slice(out.Strays, func(i, j int) bool { return out.Strays[i].Path < out.Strays[j].Path })
	return out, nil
}

func scanEngine(serveRoot, engine string, out *CatalogScan) {
	engineRoot := filepath.Join(serveRoot, engine)
	entries, err := os.ReadDir(engineRoot)
	if err != nil {
		out.Strays = append(out.Strays, Stray{
			Path:   filepath.Join("control", "serve", engine),
			Reason: "engine directory unreadable: " + err.Error(),
		})
		return
	}
	for _, e := range entries {
		name := e.Name()
		abs := filepath.Join(engineRoot, name)
		rel := filepath.Join("control", "serve", engine, name)
		isDir := e.IsDir() || (isSymlinkDir(abs))
		switch {
		case name == "rtx6000" || name == "spark":
			scanProfile(engine, name, engineRoot, out)
		case isDir:
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "engine-level experiment directory (not under a serve profile)"})
		default:
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "loose engine-level file (not a serve package)"})
		}
	}
}

func scanProfile(engine, profile, engineRoot string, out *CatalogScan) {
	base := filepath.Join(engineRoot, profile)
	targets := profileTargets[profile]
	arch := profileArch[profile]
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		abs := filepath.Join(base, name)
		rel := filepath.Join("control", "serve", engine, profile, name)
		if !artifactRe.MatchString(name) || !isDirEntry(abs) {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "not a catalog package directory"})
			continue
		}
		// catalog.py: package must resolve to a child of the canonical
		// base and carry runtime.env. Path.resolve is always absolute;
		// EvalSymlinks on a relative input stays relative, so make the
		// package path absolute before comparing.
		absFull, errA := filepath.Abs(abs)
		resolved, err := filepath.EvalSymlinks(absFull)
		canonicalBase, _ := filepath.EvalSymlinks(baseAbs)
		if errA != nil || err != nil || filepath.Dir(resolved) != canonicalBase {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "package path escapes the profile base"})
			continue
		}
		envPath := filepath.Join(abs, "runtime.env")
		if st, err := os.Stat(envPath); err != nil || !st.Mode().IsRegular() {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "missing runtime.env"})
			continue
		}
		servePath := filepath.Join(abs, "serve.sh")
		if !isExecFile(servePath) {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "missing or non-executable serve.sh"})
			continue
		}
		raw, err := os.ReadFile(envPath)
		if err != nil {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "unreadable runtime.env: " + err.Error()})
			continue
		}
		meta, err := ParseRuntimeEnv(raw)
		if err != nil {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "invalid runtime.env: " + err.Error()})
			continue
		}
		serve, err := os.ReadFile(servePath)
		if err != nil {
			out.Strays = append(out.Strays, Stray{Path: rel, Reason: "unreadable serve.sh: " + err.Error()})
			continue
		}
		pkg := SinglePackage{
			Engine:   engine,
			Profile:  profile,
			Name:     name,
			RelPath:  rel,
			AbsPath:  abs,
			Meta:     meta,
			ServeSH:  serve,
			ServeSHA: sha256Hex(serve),
			Targets:  targets,
			Arch:     arch,
		}
		if c, err := os.ReadFile(filepath.Join(abs, "capabilities.json")); err == nil {
			pkg.CapsSHA = sha256Hex(c)
		}
		pkg.Assets, _ = packageFiles(abs)
		out.Single = append(out.Single, pkg)
	}
}

func scanClusterEngine(engine string, serveRoot string, out *CatalogScan) {
	base := filepath.Join(serveRoot, "cluster", engine)
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		pkg, strays := scanClusterPackage(engine, e.Name(), filepath.Join(base, e.Name()))
		out.Strays = append(out.Strays, strays...)
		if pkg != nil {
			out.Cluster = append(out.Cluster, *pkg)
		}
	}
}

// scanClusterPackage validates one cluster package exactly as
// ServingCatalog._load does (five executable action scripts plus a strict
// runtime.env and launch profiles) and returns the parsed package, or a
// stray record explaining why it is not a catalog recipe.
func scanClusterPackage(engine, name, abs string) (*ClusterPackage, []Stray) {
	rel := filepath.Join("control", "serve", "cluster", engine, name)
	stray := func(reason string) (*ClusterPackage, []Stray) {
		return nil, []Stray{{Path: rel, Reason: reason}}
	}
	// catalog.py _packages: the resolved package must stay inside the
	// canonical engine base (symlinks escaping the tree are not recipes).
	if absFull, errA := filepath.Abs(abs); errA != nil {
		return stray("package path is not absolute")
	} else if resolved, err := filepath.EvalSymlinks(absFull); err != nil {
		return stray("package path does not resolve")
	} else if canonicalBase, cErr := filepath.EvalSymlinks(filepath.Dir(absFull)); cErr != nil || filepath.Dir(resolved) != canonicalBase {
		return stray("package path escapes the cluster engine base")
	}
	if !artifactRe.MatchString(name) || !isDirEntry(abs) {
		return stray("not a catalog cluster package directory")
	}
	for _, action := range []string{"start", "status", "logs", "verify", "stop"} {
		if !isExecFile(filepath.Join(abs, action+".sh")) {
			return stray(fmt.Sprintf("missing or non-executable %s.sh", action))
		}
	}
	raw, err := os.ReadFile(filepath.Join(abs, "runtime.env"))
	if err != nil {
		return stray("missing or unreadable runtime.env")
	}
	meta, err := ParseRuntimeEnv(raw)
	if err != nil {
		return stray("invalid runtime.env: " + err.Error())
	}
	profiles, def, err := loadLaunchProfiles(filepath.Join(abs, "profiles"))
	if err != nil {
		return stray("invalid launch profiles: " + err.Error())
	}
	pkg := &ClusterPackage{
		Engine:      engine,
		Name:        name,
		RelPath:     rel,
		AbsPath:     abs,
		Meta:        meta,
		DefaultProf: def,
		Profiles:    profiles,
	}
	pkg.Files, _ = packageFiles(abs)
	if cs, err := os.ReadFile(filepath.Join(abs, "cluster.sh")); err == nil {
		pkg.HeadHost, pkg.WorkerHost, pkg.APIPort = parseClusterConstants(string(cs))
	}
	return pkg, nil
}

// packageFiles lists every regular file under a package, sorted, with
// content digests: the read-only asset set the new package format carries.
func packageFiles(abs string) ([]FileRef, error) {
	var out []FileRef
	err := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(abs, p)
		if rerr != nil {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		out = append(out, FileRef{Path: filepath.ToSlash(rel), SHA256: sha256Hex(raw)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// isDirEntry reports whether p is a directory, following symlinks.
func isDirEntry(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isSymlinkDir(p string) bool {
	ls, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if ls.Mode()&os.ModeSymlink == 0 {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isExecFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fileSHA256 digests one file; empty on error.
func fileSHA256(p string) (string, int64) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n
}

// clusterConstant extracts one `NAME=value` assignment from cluster.sh.
func clusterConstant(script, name string) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func parseClusterConstants(script string) (head, worker string, port int) {
	head = clusterConstant(script, "HEAD_HOST")
	worker = clusterConstant(script, "WORKER_HOST")
	portStr := clusterConstant(script, "API_PORT")
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return head, worker, 0
		}
	}
	port = 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return head, worker, port
}
