package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// legacyTerminalStates are the legacy run states that admit import.
// Everything else is nonterminal: the old manager still owns it.
var legacyTerminalStates = map[string]bool{
	"succeeded":      true,
	"failed":         true,
	"cancelled":      true,
	"interrupted":    true,
	"timed_out":      true,
	"launch_failed":  true,
	"cleanup_failed": true,
}

// stateMap is the legacy → new run state transition (plan 8.5): the three
// states that exist in both map to themselves; the three legacy failure
// flavors fold into failed with a legacy_* error marker.
var stateMap = map[string]string{
	"succeeded":      "succeeded",
	"cancelled":      "cancelled",
	"interrupted":    "interrupted",
	"failed":         "failed",
	"timed_out":      "failed",
	"launch_failed":  "failed",
	"cleanup_failed": "failed",
}

var legacyMarker = map[string]string{
	"timed_out":      "legacy_timed_out",
	"launch_failed":  "legacy_launch_failed",
	"cleanup_failed": "legacy_cleanup_failed",
}

var runUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// legacyMeta is the schema-1 runs/<uuid>/meta.json document.
type legacyMeta struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	ErrorCode  *string         `json:"error_code"`
	ExitCode   *int            `json:"exit_code"`
	Schema     int             `json:"schema"`
	CreatedAt  string          `json:"created_at"`
	StartedAt  *string         `json:"started_at"`
	FinishedAt *string         `json:"finished_at"`
	Request    json.RawMessage `json:"request"`
	Resources  []string        `json:"resources"`
	Results    json.RawMessage `json:"results"`
}

// ScanRuns classifies every legacy run in stateDir/runs/<uuid>/meta.json.
// recipeDigests maps "engine/artifact/target" to a converted recipe digest
// for the request → recipe join; misses become legacy_identity.
func ScanRuns(stateDir string, recipeDigests map[string]string) ([]RunEntry, error) {
	root := filepath.Join(stateDir, "runs")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs: %w", err)
	}
	var out []RunEntry
	for _, e := range entries {
		if !e.IsDir() || !runUUIDRe.MatchString(e.Name()) {
			continue
		}
		abs := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(abs, "meta.json"))
		if err != nil {
			out = append(out, RunEntry{ID: e.Name(), LegacyState: "missing_meta", Nonterminal: true,
				ErrorCode: "legacy_meta_missing"})
			continue
		}
		var m legacyMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			out = append(out, RunEntry{ID: e.Name(), LegacyState: "bad_meta", Nonterminal: true,
				ErrorCode: "legacy_meta_invalid"})
			continue
		}
		re := RunEntry{
			ID:          m.ID,
			Kind:        m.Kind,
			LegacyState: m.State,
			CreatedAt:   m.CreatedAt,
			Request:     m.Request,
			Resources:   m.Resources,
			ExitCode:    m.ExitCode,
		}
		if m.StartedAt != nil {
			re.StartedAt = *m.StartedAt
		}
		if m.FinishedAt != nil {
			re.FinishedAt = *m.FinishedAt
		}
		if lg, err := os.ReadFile(filepath.Join(abs, "output.log")); err == nil {
			re.LogSize = int64(len(lg))
			re.LogSHA256 = sha256Hex(lg)
		}
		if !legacyTerminalStates[m.State] {
			re.Nonterminal = true
			out = append(out, re)
			continue
		}
		re.State = stateMap[m.State]
		if marker, ok := legacyMarker[m.State]; ok {
			re.ErrorCode = marker
		} else if m.ErrorCode != nil && *m.ErrorCode != "" {
			re.ErrorCode = *m.ErrorCode
		}
		if d, ok := recipeDigests[requestKey(m.Request)]; ok {
			re.RecipeDigest = d
		} else {
			re.LegacyIdentity = m.ID
		}
		out = append(out, re)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// requestKey renders the legacy request's (engine, artifact, target) join
// key; requests without an artifact (benchmarks) map to "".
func requestKey(req json.RawMessage) string {
	if len(req) == 0 {
		return ""
	}
	var r struct {
		Engine   string `json:"engine"`
		Artifact string `json:"artifact"`
		Target   string `json:"target"`
	}
	if err := json.Unmarshal(req, &r); err != nil {
		return ""
	}
	if r.Artifact == "" || r.Engine == "" || r.Target == "" {
		return ""
	}
	return r.Engine + "/" + r.Artifact + "/" + r.Target
}

// NonterminalIDs lists the nonterminal run ids in sorted order.
func NonterminalIDs(rs []RunEntry) []string {
	var out []string
	for _, r := range rs {
		if r.Nonterminal {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

// legacyTargetNodes renders a legacy resource list ("target:spark2", ...)
// into the new node identity set.
func legacyTargetNodes(resources []string) []string {
	seen := map[string]bool{}
	for _, r := range resources {
		if t, ok := cutPrefix(r, "target:"); ok {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}
