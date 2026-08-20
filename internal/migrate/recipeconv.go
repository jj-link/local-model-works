package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// imageResolver resolves container image digests. Digest-pinned references
// keep their digest; mutable local tags use the daemon's image ID when
// docker is available, else a deterministic derived digest so the document
// still satisfies the schema.
type imageResolver struct {
	docker bool
	memo   map[string]ImageRef
}

func newImageResolver(docker bool) *imageResolver {
	return &imageResolver{docker: docker, memo: map[string]ImageRef{}}
}

func (r *imageResolver) resolve(ref string) ImageRef {
	if v, ok := r.memo[ref]; ok {
		return v
	}
	out := ImageRef{Reference: ref}
	if d := refPinnedDigest(ref); d != "" {
		out.Digest, out.DigestSource = d, "pinned"
	} else {
		out.Mutable = true
		if r.docker {
			if d := dockerImageID(ref); d != "" {
				out.Digest, out.DigestSource = d, "docker"
			}
		}
		if out.Digest == "" {
			out.Digest = derivedDigest("lmw/legacy-mutable-image/v1:" + ref)
			out.DigestSource = "derived"
		}
	}
	r.memo[ref] = out
	return out
}

func refPinnedDigest(ref string) string {
	i := strings.LastIndex(ref, "@sha256:")
	if i < 0 {
		return ""
	}
	d := ref[i+1:]
	if len(d) != 71 {
		return ""
	}
	return d
}

func dockerImageID(ref string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", ref).Output()
	if err != nil {
		return ""
	}
	var rows []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		return ""
	}
	id := rows[0].ID
	if strings.HasPrefix(id, "sha256:") && len(id) == 71 {
		return id
	}
	return ""
}

func derivedDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hostDirDigest digests one host-mounted model directory: the canonical
// manifest of relative path + size + content digest. Missing directory
// yields a derived (explicitly unresolvable) digest.
func hostDirDigest(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return derivedDigest("lmw/legacy-file-artifact/v1:" + dir), false
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return derivedDigest("lmw/legacy-file-artifact/v1:" + dir), false
	}
	type ent struct {
		rel  string
		size int64
		dig  string
	}
	var ents []ent
	_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(abs, p)
		dig, size := fileSHA256(p)
		ents = append(ents, ent{filepath.ToSlash(rel), size, dig})
		return nil
	})
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	h := sha256.New()
	for _, e := range ents {
		fmt.Fprintf(h, "%s %d %s\n", e.rel, e.size, e.dig)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), true
}

// commandContract is the exact-match dedup key: image, the in-container
// invocation, the full runtime environment (minus the target-specific
// container name), the published port, and the capability contract.
type commandContract struct {
	Image         string `json:"image"`
	ImageDigest   string `json:"image_digest"`
	Served        string `json:"served"`
	Model         string `json:"model"`
	ModelRev      string `json:"model_revision"`
	ModelHost     string `json:"model_host_path"`
	Drafter       string `json:"drafter"`
	DrafterRev    string `json:"drafter_revision"`
	DrafterHost   string `json:"drafter_host_path"`
	Tokenizer     string `json:"tokenizer"`
	TokenizerRev  string `json:"tokenizer_revision"`
	TokenizerHost string `json:"tokenizer_host_path"`
	ServeSHA      string `json:"serve_sh_sha256"`
	CapsSHA       string `json:"capabilities_sha256"`
	Port          int    `json:"port"`
}

func contractOf(p SinglePackage, img ImageRef) (commandContract, json.RawMessage) {
	c := commandContract{
		Image:         p.Meta.Image,
		ImageDigest:   img.Digest,
		Served:        p.Meta.Served,
		Model:         p.Meta.Model,
		ModelRev:      p.Meta.ModelRevision,
		ModelHost:     p.Meta.ModelHostPath,
		Drafter:       p.Meta.Drafter,
		DrafterRev:    p.Meta.DrafterRevision,
		DrafterHost:   p.Meta.DrafterHostPath,
		Tokenizer:     p.Meta.Tokenizer,
		TokenizerRev:  p.Meta.TokenizerRev,
		TokenizerHost: p.Meta.TokenizerHost,
		ServeSHA:      p.ServeSHA,
		CapsSHA:       p.CapsSHA,
		Port:          8000,
	}
	b, _ := cjson.Marshal(c)
	return c, b
}

// ConvertRecipes turns the catalog's single-node packages into v1alpha1
// recipe documents. Target copies of one package (rtx6000/spark) merge into
// one capability-selected recipe only when MODEL + MODEL_REVISION + the
// command contract match exactly; otherwise they stay distinct recipes.
func ConvertRecipes(catalog *CatalogScan, resolver *imageResolver, legacyRevision string, legacyRoot string) []RecipeEntry {
	groups := map[string][]SinglePackage{}
	var order []string
	for _, p := range catalog.Single {
		key := p.Engine + "/" + p.Name
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	sort.Strings(order)

	var out []RecipeEntry
	for _, key := range order {
		members := groups[key]
		// Deterministic member order: rtx6000 before spark.
		sort.Slice(members, func(i, j int) bool { return members[i].Profile < members[j].Profile })
		var contracts []json.RawMessage
		same := true
		var firstContract json.RawMessage
		for i, m := range members {
			img := resolver.resolve(m.Meta.Image)
			_, c := contractOf(m, img)
			if i == 0 {
				firstContract = c
			} else if string(c) != string(firstContract) {
				same = false
			}
			contracts = append(contracts, c)
		}
		if same && len(members) > 1 {
			out = append(out, mergedRecipe(members, firstContract, resolver, legacyRevision, legacyRoot))
		} else {
			for _, m := range members {
				out = append(out, singleRecipe(m, resolver, legacyRevision, legacyRoot))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// recipeName normalizes a legacy package name into the schema's DNS-style
// recipe name (underscores become hyphens).
func recipeName(pkgName string) string {
	return strings.ReplaceAll(pkgName, "_", "-")
}

func modelArtifact(p SinglePackage) map[string]any {
	if p.Meta.ModelHostPath != "" {
		dig, exists := hostDirDigest(p.Meta.ModelHostPath)
		_ = exists
		return map[string]any{
			"name": "model",
			"kind": "model",
			"source": map[string]any{
				"type":     "file",
				"identity": strings.TrimPrefix(p.Meta.ModelHostPath, "/"),
				"digest":   dig,
			},
			"mount":      p.Meta.Model,
			"validation": "directory",
		}
	}
	org, repo, _ := strings.Cut(p.Meta.Model, "/")
	return map[string]any{
		"name": "model",
		"kind": "model",
		"source": map[string]any{
			"type":     "huggingface",
			"identity": "hf://" + org + "/" + repo,
			"revision": p.Meta.ModelRevision,
		},
		"mount":      "/models/hub/models--" + strings.ReplaceAll(org, "/", "--") + "--" + repo,
		"validation": "snapshot",
	}
}

func optionalArtifact(name, value, revision, hostPath string) map[string]any {
	if value == "" {
		return nil
	}
	if hostPath != "" {
		dig, _ := hostDirDigest(hostPath)
		return map[string]any{
			"name": name,
			"kind": "model",
			"source": map[string]any{
				"type":     "file",
				"identity": strings.TrimPrefix(hostPath, "/"),
				"digest":   dig,
			},
			"mount":      value,
			"validation": "directory",
		}
	}
	org, repo, _ := strings.Cut(value, "/")
	return map[string]any{
		"name": name,
		"kind": "model",
		"source": map[string]any{
			"type":     "huggingface",
			"identity": "hf://" + org + "/" + repo,
			"revision": revision,
		},
		"mount":      "/models/hub/models--" + strings.ReplaceAll(org, "/", "--") + "--" + repo,
		"validation": "snapshot",
	}
}

// modelPath is the in-container model path the legacy entrypoint
// expects: run-single.sh bind-mounts the HF repo directory at
// /models/hub/models--<org>--<repo> and hands the snapshot directory to
// the script; host-mounted models keep their literal container path.
func modelPath(p SinglePackage) string {
	if p.Meta.ModelHostPath != "" {
		return p.Meta.Model
	}
	return hfContainerPath(p.Meta.Model, p.Meta.ModelRevision)
}

func drafterPath(p SinglePackage) string {
	if p.Meta.DrafterHostPath != "" {
		return p.Meta.Drafter
	}
	return hfContainerPath(p.Meta.Drafter, p.Meta.DrafterRevision)
}

func tokenizerPath(p SinglePackage) string {
	if p.Meta.TokenizerHost != "" {
		return p.Meta.Tokenizer
	}
	return hfContainerPath(p.Meta.Tokenizer, p.Meta.TokenizerRev)
}

func hfContainerPath(repo, revision string) string {
	org, name, _ := strings.Cut(repo, "/")
	return "/models/hub/models--" + org + "--" + name + "/snapshots/" + revision
}

func workloadFor(p SinglePackage, img ImageRef) map[string]any {
	env := map[string]string{
		"MODEL_PATH": modelPath(p),
		"SERVED":     p.Meta.Served,
	}
	if p.Meta.Drafter != "" {
		env["DRAFTER_PATH"] = drafterPath(p)
	}
	if p.Meta.Tokenizer != "" {
		env["TOKENIZER_PATH"] = tokenizerPath(p)
	}
	w := map[string]any{
		"image":   map[string]any{"reference": img.Reference, "digest": img.Digest},
		"command": []string{"/bin/bash"},
		"args":    []string{"/lmw/assets/serve.sh"},
		"env":     env,
		"ports": []map[string]any{
			{"container": 8000, "host": 8000, "protocol": "tcp"},
		},
		"networkMode": "bridge",
		"resources": map[string]any{
			"cpu":         16,
			"memoryBytes": int64(128 << 30),
			"shmBytes":    int64(32 << 30),
			"tmpfsBytes":  int64(16 << 30),
			"pids":        4096,
		},
		"devices":     map[string]any{"accelerator": map[string]any{"all": true}},
		"permissions": []string{"memory.shm-large"},
		"readiness":   map[string]any{"httpGet": map[string]any{"path": "/health", "port": 8000, "method": "GET"}},
	}
	if p.CapsSHA != "" {
		// capabilities.json present: the legacy verify contract probes the
		// OpenAI surface before accepting the endpoint.
		w["verify"] = map[string]any{
			"httpGet": map[string]any{"path": "/v1/models", "port": 8000, "method": "GET"},
			"expect":  map[string]any{"statusCode": 200},
		}
	}
	return w
}

func artifactsFor(p SinglePackage) []map[string]any {
	arts := []map[string]any{modelArtifact(p)}
	if d := optionalArtifact("drafter", p.Meta.Drafter, p.Meta.DrafterRevision, p.Meta.DrafterHostPath); d != nil {
		arts = append(arts, d)
	}
	if t := optionalArtifact("tokenizer", p.Meta.Tokenizer, p.Meta.TokenizerRev, p.Meta.TokenizerHost); t != nil {
		arts = append(arts, t)
	}
	return arts
}

func assetList(p SinglePackage) []string {
	out := make([]string, 0, len(p.Assets))
	for _, a := range p.Assets {
		out = append(out, a.Path)
	}
	return out
}

func sourceDoc(legacyRoot string, pkgs []SinglePackage, legacyRevision string) map[string]any {
	src := map[string]any{
		"url":  "file://" + filepath.Join(legacyRoot, pkgs[0].RelPath),
		"path": pkgs[0].RelPath,
	}
	if legacyRevision != "" {
		src["revision"] = legacyRevision
	}
	return src
}

// buildDocument assembles one v1alpha1 recipe document for a set of
// capability-variant packages (one or more, same contract).
func buildDocument(engine string, pkgs []SinglePackage, resolver *imageResolver, legacyRevision, legacyRoot string) (json.RawMessage, error) {
	metas := make([]ImageRef, len(pkgs))
	for i, p := range pkgs {
		metas[i] = resolver.resolve(p.Meta.Image)
	}
	var archs []string
	workloads := make([]map[string]any, 0, len(pkgs))
	for i, p := range pkgs {
		w := workloadFor(p, metas[i])
		if len(pkgs) > 1 {
			w["match"] = map[string]any{"accelerator": map[string]any{"architectures": []string{p.Arch}}}
		}
		workloads = append(workloads, w)
		archs = append(archs, p.Arch)
	}
	// Distinct architectures, stable order sm_120 before sm_121.
	sort.Strings(archs)
	seen := map[string]bool{}
	var dArchs []string
	for _, a := range archs {
		if !seen[a] {
			seen[a] = true
			dArchs = append(dArchs, a)
		}
	}

	doc := map[string]any{
		"apiVersion": recipe.APIVersion,
		"kind":       "Recipe",
		"metadata": map[string]any{
			"name":        recipeName(pkgs[0].Name),
			"version":     "1.0.0",
			"displayName": pkgs[0].Meta.Served,
			"description": "Migrated from DGX-Dashboard " + engine + " package(s) " + strings.Join(pkgNames(pkgs), ", ") + ".",
			"license":     "Apache-2.0",
			"source":      sourceDoc(legacyRoot, pkgs, legacyRevision),
		},
		"compatibility": map[string]any{
			"nodeCount":   1,
			"accelerator": map[string]any{"vendor": "nvidia", "architectures": dArchs, "count": 1},
		},
		"artifacts": artifactsFor(pkgs[0]),
		"workloads": workloads,
	}
	// Assets: the union of package files (identical packages list the same
	// set; differing file sets keep the first package's assets and the
	// divergence already split the recipe).
	doc["assets"] = assetList(pkgs[0])
	canon, err := recipe.Canonical(mustJSON(doc))
	if err != nil {
		return nil, err
	}
	return canon, nil
}

func pkgNames(ps []SinglePackage) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.RelPath
	}
	return out
}

func singleRecipe(p SinglePackage, resolver *imageResolver, legacyRevision, legacyRoot string) RecipeEntry {
	img := resolver.resolve(p.Meta.Image)
	doc, err := buildDocument(p.Engine, []SinglePackage{p}, resolver, legacyRevision, legacyRoot)
	if err != nil {
		panic(err)
	}
	dig, _ := recipe.DigestOf(doc)
	_, c := contractOf(p, img)
	return RecipeEntry{
		Name:          recipeName(p.Name),
		Engine:        p.Engine,
		Packages:      []string{p.RelPath},
		Targets:       append([]string{}, p.Targets...),
		Architectures: []string{p.Arch},
		Served:        p.Meta.Served,
		Model:         p.Meta.Model,
		ModelRevision: p.Meta.ModelRevision,
		Image:         img,
		Contract:      c,
		Digest:        dig,
		Document:      doc,
	}
}

// mergedRecipe joins exact-contract target copies into one recipe with
// capability-selected variants (rtx6000/sm_120 then spark/sm_121).
func mergedRecipe(pkgs []SinglePackage, contract json.RawMessage, resolver *imageResolver, legacyRevision, legacyRoot string) RecipeEntry {
	doc, err := buildDocument(pkgs[0].Engine, pkgs, resolver, legacyRevision, legacyRoot)
	if err != nil {
		panic(err)
	}
	dig, _ := recipe.DigestOf(doc)
	paths := make([]string, len(pkgs))
	targets := map[string]bool{}
	archs := map[string]bool{}
	for i, p := range pkgs {
		paths[i] = p.RelPath
		for _, t := range p.Targets {
			targets[t] = true
		}
		archs[p.Arch] = true
	}
	tlist, alist := sortedKeys(targets), sortedKeys(archs)
	img := resolver.resolve(pkgs[0].Meta.Image)
	return RecipeEntry{
		Name:          recipeName(pkgs[0].Name),
		Engine:        pkgs[0].Engine,
		Packages:      paths,
		Targets:       tlist,
		Architectures: alist,
		Served:        pkgs[0].Meta.Served,
		Model:         pkgs[0].Meta.Model,
		ModelRevision: pkgs[0].Meta.ModelRevision,
		Image:         img,
		Merged:        true,
		Contract:      contract,
		Digest:        dig,
		Document:      doc,
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// dockerTimeout bounds one daemon query for a mutable image ID.
var dockerTimeout = 10 * time.Second
