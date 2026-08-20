package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// profile field order and value rules mirror catalog.py _load_launch_profiles.
var (
	profileFields   = []string{"KV_CACHE_DTYPE", "MAX_MODEL_LEN", "MAX_NUM_SEQS", "MTP_NUM_TOKENS"}
	launchProfileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	profileValueRe  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// loadLaunchProfiles parses a cluster package profiles/ directory with the
// catalog's strictness: a single-line default file naming one of the .env
// profiles, and each .env carrying exactly the four catalog fields in
// order with valid values.
func loadLaunchProfiles(root string) ([]ClusterProfile, string, error) {
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return nil, "", nil // no profiles directory: none
	}
	raw, err := os.ReadFile(filepath.Join(root, "default"))
	if err != nil {
		return nil, "", fmt.Errorf("default launch profile unreadable: %w", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasSuffix(text, "\n") || strings.Count(text, "\n") != 1 {
		return nil, "", fmt.Errorf("default launch profile is invalid")
	}
	def := strings.TrimSuffix(text, "\n")
	if !launchProfileRe.MatchString(def) {
		return nil, "", fmt.Errorf("default launch profile is invalid")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, "", fmt.Errorf("read profiles: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".env")
		if !launchProfileRe.MatchString(stem) {
			return nil, "", fmt.Errorf("launch profile file is unsafe: %s", e.Name())
		}
		names = append(names, stem)
	}
	sort.Strings(names)
	var out []ClusterProfile
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(root, name+".env"))
		if err != nil {
			return nil, "", fmt.Errorf("profile %s unreadable: %w", name, err)
		}
		vals := map[string]string{}
		var order []string
		for _, line := range strings.Split(string(raw), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			for _, f := range profileFields {
				if key == f {
					if _, dup := vals[f]; dup {
						return nil, "", fmt.Errorf("profile %s contains duplicate %s", name, f)
					}
					vals[f] = value
					order = append(order, f)
					break
				}
			}
		}
		if len(order) != len(profileFields) {
			return nil, "", fmt.Errorf("profile %s metadata is invalid", name)
		}
		for i, f := range profileFields {
			if order[i] != f {
				return nil, "", fmt.Errorf("profile %s metadata is invalid", name)
			}
		}
		if !profileValueRe.MatchString(vals["KV_CACHE_DTYPE"]) {
			return nil, "", fmt.Errorf("profile %s metadata is invalid", name)
		}
		cl, err := profileInt(vals["MAX_MODEL_LEN"], 1, 10000000)
		if err != nil {
			return nil, "", fmt.Errorf("profile %s: %w", name, err)
		}
		ms, err := profileInt(vals["MAX_NUM_SEQS"], 1, 1024)
		if err != nil {
			return nil, "", fmt.Errorf("profile %s: %w", name, err)
		}
		st, err := profileInt(vals["MTP_NUM_TOKENS"], 0, 32)
		if err != nil {
			return nil, "", fmt.Errorf("profile %s: %w", name, err)
		}
		out = append(out, ClusterProfile{
			Name:              name,
			KVCacheDtype:      vals["KV_CACHE_DTYPE"],
			ContextLength:     cl,
			MaxSequences:      ms,
			SpeculativeTokens: st,
		})
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("no launch profiles")
	}
	found := false
	for _, p := range out {
		if p.Name == def {
			found = true
			break
		}
	}
	if !found {
		return nil, "", fmt.Errorf("default launch profile does not exist")
	}
	return out, def, nil
}

func profileInt(value string, min, max int) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("empty integer")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %s", value)
		}
	}
	n := 0
	for _, c := range value {
		n = n*10 + int(c-'0')
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d outside supported range", n)
	}
	return n, nil
}

// ConvertClusterDrafts renders each catalog cluster package as a ranked
// workload conversion draft. Cluster recipes are hand-converted per the
// migration plan; the draft carries the full legacy contract (files with
// digests, rank/host assignment, profiles, capabilities) so the generated
// container specs can be compared against it later.
func ConvertClusterDrafts(catalog *CatalogScan, resolver *imageResolver) []ClusterDraft {
	out := make([]ClusterDraft, 0, len(catalog.Cluster))
	for _, c := range catalog.Cluster {
		img := resolver.resolve(c.Meta.Image)
		d := ClusterDraft{
			Name:           c.Name,
			Engine:         c.Engine,
			Served:         c.Meta.Served,
			ContainerName:  c.Meta.ContainerName,
			Image:          img,
			Model:          c.Meta.Model,
			ModelRevision:  c.Meta.ModelRevision,
			HeadHost:       c.HeadHost,
			WorkerHost:     c.WorkerHost,
			APIPort:        c.APIPort,
			DefaultProfile: c.DefaultProf,
			Profiles:       c.Profiles,
			Files:          c.Files,
			Ranks: []ClusterRank{
				{Rank: 0, Host: c.HeadHost, ContainerName: c.Meta.ContainerName + "-rank0"},
				{Rank: 1, Host: c.WorkerHost, ContainerName: c.Meta.ContainerName + "-rank1"},
			},
		}
		if raw, err := os.ReadFile(filepath.Join(c.AbsPath, "capabilities.json")); err == nil {
			d.Capabilities = raw
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Engine+"/"+out[i].Name < out[j].Engine+"/"+out[j].Name
	})
	return out
}
