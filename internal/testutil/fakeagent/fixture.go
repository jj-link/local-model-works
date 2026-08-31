package fakeagent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/hf"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// ImageDigests are the deterministic digest-pinned images used by every
// fixture recipe (the validator rejects mutable tags and missing digests).
const (
	ImageServe    = "ghcr.io/localmodelworks/spark-serve:1.0.0"
	ImageServeDig = "sha256:" + "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
	ImageSmall    = "ghcr.io/localmodelworks/worker:1.0.0"
	ImageSmallDig = "sha256:" + "b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1"
)

// FixtureRecipe is one installable test recipe.
type FixtureRecipe struct {
	Name        string
	Version     string
	NodeCount   int
	GPUsPerRank int    // 0 = no accelerator requirement
	Fabric      string // "" | "roce"
	Artifacts   []recipe.Artifact
	Port        int // 0 = none; rank r publishes Port+r
	Profiles    map[string]any
}

func (r FixtureRecipe) Manifest() recipe.Manifest {
	// The recipe schema requires "artifacts" as an array (minItems 0); a nil
	// slice would marshal to null and fail validation.
	if r.Artifacts == nil {
		r.Artifacts = []recipe.Artifact{}
	}
	img := ImageSmall
	dig := ImageSmallDig
	if r.GPUsPerRank > 0 {
		img, dig = ImageServe, ImageServeDig
	}
	compat := recipe.Compatibility{NodeCount: r.NodeCount}
	if r.GPUsPerRank > 0 {
		compat.Accelerator = &recipe.AccCompat{
			Vendor: "nvidia", Count: r.GPUsPerRank, MinMemoryBytes: 8 * 1024 * 1024 * 1024,
		}
	}
	if r.Fabric != "" {
		compat.Fabric = &recipe.FabricCompat{Transport: r.Fabric, MinBandwidthGbps: 100}
	}
	args := []string{"--rank", "${node.rank}"}
	if r.Port > 0 {
		args = append(args, "--addr", "${node.address}", "--port", fmt.Sprintf("%d", r.Port))
	}
	for _, a := range r.Artifacts {
		args = append(args, "--model", "${artifact."+a.Name+".path}")
	}
	w := recipe.Workload{
		Image:   recipe.Image{Reference: img, Digest: dig},
		Command: []string{"/opt/serve"},
		Args:    args,
		Resources: recipe.Resources{
			CPU: 2, MemoryBytes: 4 * 1024 * 1024 * 1024, Pids: 512,
		},
	}
	// Multi-node recipes must declare ranks (recipe validator); the single
	// variant serves every rank, one per node.
	if r.NodeCount > 1 {
		w.Ranks = make([]int, r.NodeCount)
		for i := range w.Ranks {
			w.Ranks[i] = i
		}
	}
	if r.GPUsPerRank > 0 {
		w.Devices = &recipe.Devices{Accelerator: &recipe.DevAcc{All: true}}
		w.NetworkMode = "host"
		w.Permissions = []string{"network.host"}
	}
	if r.Fabric != "" {
		w.Devices = mergeDevices(w.Devices, &recipe.Devices{RDMA: &recipe.DevRdma{All: true}})
		w.Permissions = append(w.Permissions, "devices.rdma")
	}
	if r.Port > 0 {
		w.Ports = []recipe.Port{{Container: r.Port, Protocol: "tcp"}}
	}
	m := recipe.Manifest{
		APIVersion: recipe.APIVersion,
		Kind:       "Recipe",
		Metadata: recipe.Metadata{
			Name: r.Name, Version: r.Version,
			Description: "fakeagent fixture recipe", License: "MIT",
			Source: &recipe.Source{
				URL: "https://fixtures.local/fakeagent", Revision: strings.Repeat("0", 40), Path: ".",
			},
		},
		Compatibility: compat,
		Artifacts:     r.Artifacts,
		Parameters: []recipe.Parameter{{
			Name: "model", Type: "string",
			Default:     "acme/test-model",
			Description: "served model identity",
		}},
		Workloads: []recipe.Workload{w},
	}
	return m
}

func mergeDevices(base, extra *recipe.Devices) *recipe.Devices {
	if base == nil {
		return extra
	}
	out := *base
	out.RDMA = extra.RDMA
	return &out
}

// Install validates r with the real schema validator (strict: zero
// diagnostics) and stores the canonical manifest in the recipe store as
// trust=local. Returns the content digest and the parsed manifest.
func InstallRecipe(t *testing.T, s *Server, r FixtureRecipe) (string, *recipe.Manifest) {
	t.Helper()
	v, err := recipe.NewValidator()
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	m := r.Manifest()
	doc := mustJSONRecipe(m)
	if _, diags, err := v.ValidateStrict(doc); err != nil {
		t.Fatalf("recipe %s: %v", r.Name, err)
	} else if len(diags) > 0 {
		for _, d := range diags {
			t.Logf("recipe %s diag: %s %s %s", r.Name, d.Code, d.Severity, d.Message)
		}
		t.Fatalf("recipe %s: %d validation diagnostics", r.Name, len(diags))
	}
	canon, err := recipe.Canonical(doc)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	parsed, err := recipe.Parse(canon)
	if err != nil {
		t.Fatalf("parse canonical: %v", err)
	}
	stored, err := s.Srv.Env().Recipes.Store(
		s.Ctx, canon, recipe.RecipeSource{Type: "local", Path: "fakeagent"},
	)
	if err != nil {
		t.Fatalf("store recipe: %v", err)
	}
	return stored.Digest, parsed
}

func mustJSONRecipe(m recipe.Manifest) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// HuggingFace-style fixture tree
// ---------------------------------------------------------------------------

// HFFixture describes the deterministic model tree built by BuildHFFixture.
type HFFixture struct {
	Root       string // cache root (the agent's CacheRoots entry)
	ModelDir   string // <root>/models--acme--test-model
	Snapshot   string // <ModelDir>/snapshots/<sha40>
	Sha40      string
	ShardSize  int64
	ShardA     string // first real shard path (inside snapshot)
	ShardBLink string // second shard: relative symlink into blobs/
	IndexFile  string // snapshot/model.safetensors.index.json
	SymlinkIn  string // snapshot/extra/pointer.json -> ../model.safetensors.index.json
	BlobFile   string // <ModelDir>/blobs/sha256-<64>
}

// BuildHFFixture writes an HF-hub-style tree under root:
//
//	models--acme--test-model/
//	  refs/main                          (40-hex sha)
//	  blobs/sha256-<64hex>                (deterministic bytes, shard B)
//	  snapshots/<sha40>/
//	    config.json
//	    model-00001-of-00002.safetensors (deterministic bytes, shard A)
//	    model-00002-of-00002.safetensors -> ../../blobs/sha256-<64hex>
//	    model.safetensors.index.json      (weight_map -> both shards)
//	    extra/pointer.json                -> ../model.safetensors.index.json
//
// The tree passes hf.ValidateSnapshot with zero diagnostics.
func BuildHFFixture(t *testing.T, root string, shardSize int64) *HFFixture {
	t.Helper()
	sha40 := "6c47916f85e52b5e712223ca8f93952f90255714"
	blobName := "sha256-" + blobHex(t, shardSize, 2)
	fx := &HFFixture{
		Root:       root,
		ModelDir:   filepath.Join(root, "models--acme--test-model"),
		Snapshot:   filepath.Join(root, "models--acme--test-model", "snapshots", sha40),
		Sha40:      sha40,
		ShardSize:  shardSize,
		ShardA:     filepath.Join(root, "models--acme--test-model", "snapshots", sha40, "model-00001-of-00002.safetensors"),
		ShardBLink: filepath.Join(root, "models--acme--test-model", "snapshots", sha40, "model-00002-of-00002.safetensors"),
		IndexFile:  filepath.Join(root, "models--acme--test-model", "snapshots", sha40, "model.safetensors.index.json"),
		SymlinkIn:  filepath.Join(root, "models--acme--test-model", "snapshots", sha40, "extra", "pointer.json"),
		BlobFile:   filepath.Join(root, "models--acme--test-model", "blobs", blobName),
	}
	dirs := []string{
		fx.ModelDir + "/refs", fx.ModelDir + "/blobs", fx.Snapshot + "/extra",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("fixture dirs: %v", err)
		}
	}
	writeBytes(t, fx.ModelDir+"/refs/main", []byte(sha40+"\n"))
	writeBytes(t, fx.Snapshot+"/config.json", []byte(`{"model_type":"acme-test","n_shards":2}`+"\n"))
	writeShard(t, fx.ShardA, shardSize, 1)
	writeShard(t, fx.BlobFile, shardSize, 2)
	if err := os.Symlink("../../blobs/"+blobName, fx.ShardBLink); err != nil {
		t.Fatalf("shard B symlink: %v", err)
	}
	if err := os.Symlink("../model.safetensors.index.json", fx.SymlinkIn); err != nil {
		t.Fatalf("pointer symlink: %v", err)
	}
	idx := fmt.Sprintf(`{"metadata":{"total_size":%d},"weight_map":{"layer.0":"model-00001-of-00002.safetensors","layer.1":"model-00002-of-00002.safetensors"}}%s`,
		2*shardSize, "\n")
	writeBytes(t, fx.IndexFile, []byte(idx))

	// The fixture must pass the product's snapshot validator.
	if diags := hf.ValidateSnapshot(fx.Snapshot, fx.ModelDir); len(diags) > 0 {
		for _, d := range diags {
			t.Logf("fixture validation: %s %s", d.Code, d.Message)
		}
		t.Fatalf("HF fixture fails validation: %d diagnostics", len(diags))
	}
	return fx
}

// Validate passes the product snapshot validator on p (root = containment).
func Validate(p, root string) []hf.Diagnostic { return hf.ValidateSnapshot(p, root) }

// writeShard writes deterministic pseudo-random bytes (seeded per shard).
func writeShard(t *testing.T, path string, size int64, seed uint64) {
	t.Helper()
	b := make([]byte, size)
	x := seed
	for i := 0; i+8 <= len(b); i += 8 {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		binary.LittleEndian.PutUint64(b[i:], z)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write shard %s: %v", path, err)
	}
}

// blobHex is the sha256 of shard 2's content (deterministic).
func blobHex(t *testing.T, size int64, seed uint64) string {
	t.Helper()
	b := make([]byte, size)
	x := seed
	for i := 0; i+8 <= len(b); i += 8 {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		binary.LittleEndian.PutUint64(b[i:], z)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TreeInventory is one file of a tree walk: resolved size and content hash.
type TreeInventory struct {
	Path string // relative slash path
	Size int64
	SHA  string
}

// WalkTree inventories a tree dereferencing symlinks (relative paths,
// slash-separated, sorted). Symlinks are reported as symlinks when kept.
func WalkTree(t *testing.T, root string) []TreeInventory {
	t.Helper()
	var out []TreeInventory
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := os.Stat(p) // follows symlinks
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out = append(out, TreeInventory{
			Path: strings.ReplaceAll(rel, "\\", "/"),
			Size: fi.Size(),
			SHA:  hex.EncodeToString(sum[:]),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// CompareTrees asserts two trees are identical file-for-file (resolved
// content digests and sizes); fails with a human diff on mismatch.
func CompareTrees(t *testing.T, aName string, a []TreeInventory, bName string, b []TreeInventory) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("tree mismatch: %s has %d files, %s has %d", aName, len(a), bName, len(b))
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Size != b[i].Size || a[i].SHA != b[i].SHA {
			t.Fatalf("tree mismatch at %d: %s{%s %d} vs %s{%s %d}",
				i, aName, a[i].Path, a[i].Size, bName, b[i].Path, b[i].Size)
		}
	}
}
