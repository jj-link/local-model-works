// Service: the installed-recipe store with import from catalog, OCI
// reference, pinned Git, and local directories.
package recipe

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
)

var (
	ErrUnknown   = errors.New("unknown recipe")
	ErrReference = errors.New("recipe referenced")
	// ErrUnpinnedRevision — a non-empty git source revision is not a full
	// current default-branch HEAD before cloning and is persisted immutably.
	// Stable API code: recipe.unpinned_revision (422).
	ErrUnpinnedRevision = errors.New("unpinned git revision")
)

// RecipeSource mirrors the openapi RecipeImport source.
type RecipeSource struct {
	Type      string `json:"type"`
	Reference string `json:"reference,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Path      string `json:"path,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Tree      string `json:"tree,omitempty"`
}

// Recipe is the API view (openapi Recipe).
type Recipe struct {
	Digest        string          `json:"digest"`
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	Model         string          `json:"model,omitempty"`
	Engine        string          `json:"engine,omitempty"`
	Description   string          `json:"description,omitempty"`
	License       string          `json:"license,omitempty"`
	Source        json.RawMessage `json:"source"`
	Permissions   []string        `json:"permissions,omitempty"`
	Compatibility json.RawMessage `json:"compatibility,omitempty"`
	ArtifactCount int             `json:"artifact_count"`
	ProfileCount  int             `json:"profile_count"`
	VariantCount  int             `json:"variant_count"`
	HighRisk      []string        `json:"high_risk,omitempty"`
	InstalledAt   string          `json:"installed_at"`
	VersionCount  int             `json:"version_count,omitempty"`
	Update        *UpdateStatus   `json:"update,omitempty"`
}

// RecipeDetail adds the full manifest (openapi RecipeDetail).
type RecipeDetail struct {
	Recipe
	Manifest json.RawMessage `json:"manifest"`
}

type Service struct {
	db                  *sql.DB
	q                   *db.Queries
	bus                 *events.EventBus
	validator           *Validator
	catalogSchema       *jsonschema.Schema
	catalogRoot         string
	packageRoot         string
	onInstalled         func()
	updateMu            sync.Mutex
	resolveGitHead      func(context.Context, string) (string, string, error)
	repositoryCompilers RepositoryCompilerRegistry
}

// New builds the store. packageRoot holds complete immutable OCI packages.
func New(sqlDB *sql.DB, q *db.Queries, bus *events.EventBus, v *Validator, catalogRoot, packageRoot string) (*Service, error) {
	if packageRoot == "" {
		return nil, fmt.Errorf("recipe package root is required")
	}
	catalogSchema, err := compileCatalogSchema()
	if err != nil {
		return nil, fmt.Errorf("catalog schema: %w", err)
	}
	service := &Service{
		db: sqlDB, q: q, bus: bus, validator: v, catalogSchema: catalogSchema,
		catalogRoot: catalogRoot, packageRoot: packageRoot, resolveGitHead: resolveGitHEAD,
	}
	if err := service.recoverPackageDeletes(context.Background()); err != nil {
		return nil, fmt.Errorf("recover recipe package deletion: %w", err)
	}
	return service, nil
}

func (s *Service) recoverPackageDeletes(ctx context.Context) error {
	entries, err := os.ReadDir(s.packageRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		originalName, suffix, found := strings.Cut(entry.Name(), ".deleting-")
		if !found || suffix == "" || len(originalName) != 64 {
			continue
		}
		tombstone := filepath.Join(s.packageRoot, entry.Name())
		original := filepath.Join(s.packageRoot, originalName)
		_, recipeErr := s.q.GetRecipe(ctx, "sha256:"+originalName)
		if recipeErr == nil {
			if _, statErr := os.Stat(original); statErr == nil {
				if err := RemovePackage(tombstone); err != nil {
					return err
				}
				continue
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			if err := os.Rename(tombstone, original); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(recipeErr, sql.ErrNoRows) {
			return recipeErr
		}
		if err := RemovePackage(tombstone); err != nil {
			return err
		}
	}
	return nil
}

// SetInstallHook registers the controller callback used to request cache
// rescans after a new recipe makes previously unknown placements relevant.
func (s *Service) SetInstallHook(hook func()) { s.onInstalled = hook }

// Store packages a manifest with an empty deterministic asset layer and
// persists it through the same immutable path as every import source.
func (s *Service) Store(ctx context.Context, doc []byte, source RecipeSource) (Recipe, error) {
	res, err := PackManifest(doc, map[string][]byte{}, nil)
	if err != nil {
		return Recipe{}, err
	}
	return s.storePack(ctx, res, source)
}

func (s *Service) storePack(ctx context.Context, res *PackResult, source RecipeSource) (Recipe, error) {
	return s.storePackWithCurrent(ctx, res, source, true)
}

func (s *Service) storePackWithCurrent(ctx context.Context, res *PackResult, source RecipeSource, setCurrent bool) (Recipe, error) {
	manifest, vds, err := s.validator.ValidateStrict(res.ConfigJSON)
	if err != nil {
		return Recipe{}, err
	}
	adapted := make([]diag.Diagnostic, 0, len(vds))
	for _, d := range vds {
		sev := "info"
		if d.Severity == "error" || d.Severity == "warning" {
			sev = d.Severity
		}
		adapted = append(adapted, diag.Diagnostic{Code: d.Code, Severity: sev, Message: d.Message})
	}
	if diag.HasError(adapted) {
		return Recipe{}, fmt.Errorf("recipe validation: %s", vds[0].Message)
	}
	packageDir, created, err := PersistPackage(s.packageRoot, res)
	if err != nil {
		return Recipe{}, err
	}
	keepPackage := false
	defer func() {
		if created && !keepPackage {
			_ = RemovePackage(packageDir)
		}
	}()
	digest := res.ManifestDigest
	srcJSON, err := json.Marshal(source)
	if err != nil {
		return Recipe{}, err
	}
	mj, err := json.Marshal(manifest)
	if err != nil {
		return Recipe{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Recipe{}, err
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	row, err := qtx.GetRecipe(ctx, digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Recipe{}, err
	}
	if err == nil {
		if _, linkErr := qtx.GetRecipeRepositoryVersionByDigest(ctx, digest); errors.Is(linkErr, sql.ErrNoRows) {
			if linkErr = attachRepositoryVersionWithCurrent(ctx, qtx, manifest, digest, source.Tree, row.InstalledAt, setCurrent); linkErr != nil {
				return Recipe{}, linkErr
			}
		} else if linkErr != nil {
			return Recipe{}, linkErr
		}
		if err := tx.Commit(); err != nil {
			return Recipe{}, err
		}
		keepPackage = true
		return s.render(ctx, row, manifest)
	}
	packageIdentity := "recipe://" + digest
	packageSum := sha256.Sum256([]byte(packageIdentity))
	if err := qtx.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: "artifact-" + hex.EncodeToString(packageSum[:8]), Kind: "recipe", Identity: packageIdentity,
		Digest: nullStr(digest), Metadata: "{}",
	}); err != nil {
		return Recipe{}, err
	}
	for _, artifact := range manifest.Artifacts {
		// Register the static source (no variants) or every variant's source
		// (variant artifacts), deduplicated by canonical identity so planning
		// a non-default selection finds its artifact in the library.
		sources := map[string]*ArtSource{}
		order := []string{}
		if len(artifact.Variants) == 0 {
			if artifact.Source != nil {
				s, sErr := artifact.EffectiveSource("")
				if sErr != nil {
					return Recipe{}, fmt.Errorf("artifact %s: %w", artifact.Name, sErr)
				}
				sources[fmt.Sprintf("%s/%s", s.Identity, s.Revision)] = s
				order = append(order, s.Identity+"/"+s.Revision)
			}
		} else {
			for _, v := range artifact.Variants {
				key := fmt.Sprintf("%s/%s", v.Source.Identity, v.Source.Revision)
				if _, ok := sources[key]; !ok {
					sources[key] = &v.Source
					order = append(order, key)
				}
			}
		}
		for _, key := range order {
			src := sources[key]
			canonical, err := artifactidentity.Canonical(src.Type, src.Identity, src.Revision, src.Digest)
			if err != nil {
				return Recipe{}, fmt.Errorf("artifact %s: %w", artifact.Name, err)
			}
			sum := sha256.Sum256([]byte(canonical))
			metadata, _ := json.Marshal(map[string]any{
				"name": artifact.Name, "mount": artifact.Mount, "variant": key, "size_bytes": artifact.SizeBytes,
			})
			if err := qtx.CreateArtifact(ctx, db.CreateArtifactParams{
				ID:       "artifact-" + hex.EncodeToString(sum[:8]),
				Kind:     artifact.Kind,
				Identity: canonical,
				Revision: nullStr(src.Revision),
				Digest:   nullStr(src.Digest),
				Metadata: string(metadata),
			}); err != nil {
				return Recipe{}, err
			}
		}
	}
	if err := qtx.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: manifest.Metadata.Name, Version: manifest.Metadata.Version,
		Description: nullStr(manifest.Metadata.Description), License: nullStr(manifest.Metadata.License),
		Source: string(srcJSON), Manifest: string(mj),
	}); err != nil {
		return Recipe{}, err
	}
	createdRow, err := qtx.GetRecipe(ctx, digest)
	if err != nil {
		return Recipe{}, err
	}
	if err := attachRepositoryVersionWithCurrent(ctx, qtx, manifest, digest, source.Tree, createdRow.InstalledAt, setCurrent); err != nil {
		return Recipe{}, err
	}
	if err := tx.Commit(); err != nil {
		return Recipe{}, err
	}
	keepPackage = true
	s.bus.Publish(ctx, "recipe.installed", "", mustJSON(map[string]any{
		"digest": digest, "name": manifest.Metadata.Name, "version": manifest.Metadata.Version,
	}))
	detail, err := s.Get(ctx, digest)
	if err != nil {
		return Recipe{}, err
	}
	if s.onInstalled != nil {
		s.onInstalled()
	}
	return detail.Recipe, nil
}

// Import dispatches a RecipeSource.
func (s *Service) Import(ctx context.Context, src RecipeSource) (Recipe, error) {
	switch src.Type {
	case "local":
		return s.importDir(ctx, src.Path, src)
	case "catalog":
		entry, err := s.resolveCatalog(src.Reference)
		if err != nil {
			return Recipe{}, err
		}
		immutable := entry.OCI.Reference + "@" + entry.OCI.Digest
		if strings.HasPrefix(entry.OCI.Reference, "file://") {
			dir := strings.TrimPrefix(entry.OCI.Reference, "file://")
			got, err := ReadLayoutDigest(dir)
			if err != nil || got != entry.OCI.Digest {
				return Recipe{}, fmt.Errorf("catalog package digest mismatch: got %q, want %q", got, entry.OCI.Digest)
			}
			kept := src
			kept.Reference = immutable
			kept.Path = ""
			return s.importDir(ctx, dir, kept)
		}
		if strings.Contains(entry.OCI.Reference, "@") {
			return Recipe{}, fmt.Errorf("catalog OCI reference must not contain a digest")
		}
		return s.importOCI(ctx, RecipeSource{Type: "oci", Reference: immutable})
	case "oci":
		return s.importOCI(ctx, src)
	case "git":
		return s.importGit(ctx, src)
	default:
		return Recipe{}, fmt.Errorf("unknown recipe source type %q", src.Type)
	}
}

// Get returns full detail for one digest.
func (s *Service) Get(ctx context.Context, digest string) (RecipeDetail, error) {
	row, err := s.q.GetRecipe(ctx, digest)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RecipeDetail{}, ErrUnknown
		}
		return RecipeDetail{}, err
	}
	var m *Manifest
	if err := json.Unmarshal([]byte(row.Manifest), &m); err != nil {
		return RecipeDetail{}, fmt.Errorf("recipe manifest: %w", err)
	}
	base, err := s.render(ctx, row, m)
	if err != nil {
		return RecipeDetail{}, err
	}
	return RecipeDetail{Recipe: base, Manifest: json.RawMessage(row.Manifest)}, nil
}

// List returns one current package per repository plus unlinked local packages.
// Repository versions remain addressable by digest.
func (s *Service) List(ctx context.Context) ([]Recipe, error) {
	repositories, err := s.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Recipe, 0, len(repositories))
	for _, repository := range repositories {
		if repository.Current != nil {
			out = append(out, *repository.Current)
		}
	}
	rows, err := s.q.ListUnlinkedRecipes(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var manifest *Manifest
		if err := json.Unmarshal([]byte(row.Manifest), &manifest); err != nil {
			return nil, err
		}
		rendered, err := s.render(ctx, row, manifest)
		if err != nil {
			return nil, err
		}
		out = append(out, rendered)
	}
	return out, nil
}

// Delete uninstalls one recipe; blocked while any deployment or run
// references it. ifMatch must equal the digest.
func (s *Service) Delete(ctx context.Context, digest, ifMatch string) error {
	if ifMatch != digest {
		return fmt.Errorf("%w: If-Match must equal the recipe digest", ErrUnknown)
	}
	if _, err := s.q.GetRecipe(ctx, digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknown
		}
		return err
	}
	packageDir := filepath.Join(s.packageRoot, strings.TrimPrefix(digest, "sha256:"))
	tombstone := fmt.Sprintf("%s.deleting-%d", packageDir, time.Now().UnixNano())
	renamed := false
	if err := os.Rename(packageDir, tombstone); err == nil {
		renamed = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stage recipe package removal: %w", err)
	}
	restore := func() {
		if renamed {
			_ = os.Rename(tombstone, packageDir)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		restore()
		return err
	}
	qtx := s.q.WithTx(tx)
	deps, err := qtx.RecipeReferencedByDeployments(ctx, digest)
	if err == nil && deps > 0 {
		err = fmt.Errorf("%w: %d deployment(s) reference this recipe", ErrReference, deps)
	}
	if err == nil {
		var runRefs int64
		runRefs, err = qtx.RecipeReferencedByRuns(ctx, digest)
		if err == nil && runRefs > 0 {
			err = fmt.Errorf("%w: %d run(s) reference this recipe", ErrReference, runRefs)
		}
	}
	if err == nil {
		err = detachRepositoryVersion(ctx, qtx, digest)
	}
	if err == nil {
		err = qtx.DeleteRecipe(ctx, digest)
	}
	if err != nil {
		tx.Rollback()
		restore()
		return err
	}
	if err := tx.Commit(); err != nil {
		restore()
		return err
	}
	if renamed {
		if err := RemovePackage(tombstone); err != nil {
			s.bus.Publish(ctx, "recipe.cleanup_failed", "", mustJSON(map[string]any{"digest": digest, "path": tombstone, "error": err.Error()}))
		}
	}
	s.bus.Publish(ctx, "recipe.removed", "", mustJSON(map[string]any{"digest": digest}))
	return nil
}

// importDir loads a recipe from a directory (plain recipe directory or
// on-disk OCI layout) and persists its complete package.
func (s *Service) importDir(ctx context.Context, dir string, src RecipeSource) (Recipe, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Recipe{}, fmt.Errorf("source directory: %w", err)
	}
	if !info.IsDir() {
		return Recipe{}, fmt.Errorf("source directory: %s is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		res, err := ReadLayout(dir)
		if err != nil {
			return Recipe{}, fmt.Errorf("OCI layout: %w", err)
		}
		return s.storePack(ctx, res, src)
	}
	_, res, err := PackFromDir(dir, s.validator)
	if err != nil {
		return Recipe{}, err
	}
	return s.storePack(ctx, res, src)
}

// importOCI pulls one immutable package.
func (s *Service) importOCI(ctx context.Context, src RecipeSource) (Recipe, error) {
	if src.Reference == "" {
		return Recipe{}, fmt.Errorf("oci source: reference is required")
	}
	ref, err := registry.ParseReference(src.Reference)
	if err != nil {
		return Recipe{}, fmt.Errorf("oci reference: %w", err)
	}
	if !sha256DigestRE.MatchString(ref.Reference) {
		return Recipe{}, fmt.Errorf("oci source must use an immutable sha256 digest")
	}
	repo, err := remote.NewRepository(ref.String())
	if err != nil {
		return Recipe{}, fmt.Errorf("oci reference: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	manifestDesc, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return Recipe{}, fmt.Errorf("resolve %s: %w", ref.String(), err)
	}
	manifestBytes, err := fetchDesc(ctx, repo, manifestDesc)
	if err != nil {
		return Recipe{}, fmt.Errorf("fetch manifest: %w", err)
	}
	var m ociManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return Recipe{}, fmt.Errorf("manifest: %w", err)
	}
	if m.ArtifactType != ArtifactType {
		return Recipe{}, fmt.Errorf("artifact type %q is not a recipe package", m.ArtifactType)
	}
	if m.Config.MediaType != ConfigMediaType || len(m.Layers) != 1 || m.Layers[0].MediaType != LayerMediaType {
		return Recipe{}, fmt.Errorf("recipe package must contain one config and exactly one asset layer")
	}
	if !sha256DigestRE.MatchString(m.Config.Digest) || !sha256DigestRE.MatchString(m.Layers[0].Digest) {
		return Recipe{}, fmt.Errorf("recipe package contains an invalid descriptor digest")
	}
	doc, err := fetchDesc(ctx, repo, toSpecDesc(m.Config))
	if err != nil {
		return Recipe{}, fmt.Errorf("fetch config: %w", err)
	}
	layer, err := fetchDesc(ctx, repo, toSpecDesc(m.Layers[0]))
	if err != nil {
		return Recipe{}, fmt.Errorf("fetch asset layer: %w", err)
	}
	res := &PackResult{
		ManifestDigest: manifestDesc.Digest.String(),
		ManifestJSON:   manifestBytes,
		ConfigDigest:   m.Config.Digest,
		LayerDigest:    m.Layers[0].Digest,
		ConfigSize:     int64(len(doc)),
		LayerSize:      int64(len(layer)),
		ConfigJSON:     doc,
		layerBytes:     layer,
	}
	kept := src
	kept.Reference = ref.String()
	return s.storePack(ctx, res, kept)
}

// importGit clones one exact commit into an ephemeral checkout. Only the
// validated package survives.
func (s *Service) importGit(ctx context.Context, src RecipeSource) (Recipe, error) {
	if src.Remote == "" {
		return Recipe{}, fmt.Errorf("git source: remote is required")
	}
	if strings.TrimSpace(src.Revision) == "" {
		_, commit, err := s.resolveGitHead(ctx, src.Remote)
		if err != nil {
			return Recipe{}, fmt.Errorf("resolve git HEAD: %w", err)
		}
		src.Revision = commit
	}
	if !sha40.MatchString(src.Revision) {
		return Recipe{}, fmt.Errorf("%w: %q is not a 40-hex commit", ErrUnpinnedRevision, src.Revision)
	}
	tmp, err := os.MkdirTemp("", "lmw-recipe-git-*")
	if err != nil {
		return Recipe{}, err
	}
	defer os.RemoveAll(tmp)
	if isLocalGitPath(src.Remote) {
		if err := runGit(ctx, tmp, "clone", "--no-hardlinks", src.Remote, tmp); err != nil {
			return Recipe{}, fmt.Errorf("git clone: %w", err)
		}
	} else if err := runGit(ctx, tmp, "clone", src.Remote, tmp); err != nil {
		return Recipe{}, fmt.Errorf("git clone: %w", err)
	}
	if err := runGit(ctx, tmp, "checkout", "--quiet", src.Revision); err != nil {
		return Recipe{}, fmt.Errorf("git checkout %s: %w", src.Revision, err)
	}
	commit, err := runGitOutput(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return Recipe{}, err
	}
	if commit != strings.ToLower(src.Revision) {
		return Recipe{}, fmt.Errorf("git checkout resolved %s, expected %s", commit, src.Revision)
	}
	tree, err := runGitOutput(ctx, tmp, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Recipe{}, err
	}
	if s.repositoryCompilers != nil {
		repositoryID, normalizedURL, normalizedPath, identityErr := RepositoryIdentity(Source{
			URL: src.Remote, Path: src.Path,
		})
		if identityErr != nil {
			return Recipe{}, identityErr
		}
		repositorySource := RepositorySource{
			RepositoryID: repositoryID, URL: normalizedURL, Path: normalizedPath,
			CommitSHA: commit, TreeSHA: tree,
		}
		compiler, ok := s.repositoryCompilers.Lookup(repositorySource, tmp)
		if !ok {
			return Recipe{}, &PackError{
				Code:    RepositoryUnsupportedCode,
				Message: "repository has no native recipe bundle or registered deterministic compiler",
			}
		}
		packed, compileErr := compiler.Compile(ctx, repositorySource, tmp, nil)
		if compileErr != nil {
			return Recipe{}, compileErr
		}
		kept := src
		kept.Remote = normalizedURL
		kept.Path = normalizedPath
		kept.Revision = commit
		kept.Tree = tree
		return s.storePack(ctx, packed, kept)
	}
	target := tmp
	if src.Path != "" {
		rel := filepath.Clean(src.Path)
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Recipe{}, fmt.Errorf("git source: path %q escapes the checkout", src.Path)
		}
		target = filepath.Join(tmp, rel)
	}
	kept := src
	kept.Revision = commit
	kept.Tree = tree
	return s.importDir(ctx, target, kept)
}

// fetchDesc fetches one descriptor with a media-type-specific hard bound.
func fetchDesc(ctx context.Context, repo *remote.Repository, d ocispec.Descriptor) ([]byte, error) {
	max := int64(MaxConfigBytes)
	if d.MediaType == LayerMediaType {
		max = MaxCompressedLayerBytes
	}
	if d.Size < 0 || d.Size > max {
		return nil, fmt.Errorf("descriptor %s exceeds size limit", d.Digest)
	}
	r, err := repo.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max || int64(len(raw)) != d.Size {
		return nil, fmt.Errorf("descriptor %s size mismatch", d.Digest)
	}
	return raw, nil
}

// toSpecDesc converts the package manifest descriptor to the image-spec
// descriptor type expected by the oras client.
func toSpecDesc(d ociDescriptor) ocispec.Descriptor {
	sum, _ := digest.Parse(d.Digest)
	return ocispec.Descriptor{MediaType: d.MediaType, Digest: sum, Size: d.Size}
}

// render fills the API view from a store row + parsed manifest.
func (s *Service) render(ctx context.Context, row db.Recipe, m *Manifest) (Recipe, error) {
	versionCount := 1
	if link, linkErr := s.q.GetRecipeRepositoryVersionByDigest(ctx, row.Digest); linkErr == nil {
		versions, versionsErr := s.q.ListRecipeRepositoryVersions(ctx, link.RepositoryID)
		if versionsErr != nil {
			return Recipe{}, versionsErr
		}
		versionCount = len(versions)
	} else if !errors.Is(linkErr, sql.ErrNoRows) {
		return Recipe{}, linkErr
	}
	v := Recipe{
		Digest:        row.Digest,
		Name:          visibleRecipeName(row.Source, row.Name),
		Version:       row.Version,
		Model:         m.Metadata.Model,
		Engine:        m.Metadata.Engine,
		Description:   nullStrValue(row.Description),
		License:       nullStrValue(row.License),
		Source:        json.RawMessage(row.Source),
		ArtifactCount: len(m.Artifacts),
		HighRisk:      m.HighRiskPermissions(),
		InstalledAt:   row.InstalledAt,
		VersionCount:  versionCount,
	}
	update, err := s.UpdateStatus(ctx, row.Digest)
	if err != nil {
		return Recipe{}, err
	}
	v.Update = update
	comp, err := json.Marshal(m.Compatibility)
	if err == nil && string(comp) != "null" {
		v.Compatibility = comp
	}
	if m.Profiles != nil {
		v.ProfileCount = len(m.Profiles)
	}
	v.VariantCount = len(m.Workloads)
	return v, nil
}

func visibleRecipeName(sourceJSON, fallback string) string {
	var source RecipeSource
	if json.Unmarshal([]byte(sourceJSON), &source) != nil || source.Type != "git" {
		return fallback
	}
	remote := strings.TrimSuffix(strings.TrimSuffix(source.Remote, "/"), ".git")
	remote = strings.TrimPrefix(remote, "git@github.com:")
	remote = strings.TrimPrefix(remote, "ssh://git@github.com/")
	remote = strings.TrimPrefix(remote, "https://github.com/")
	remote = strings.TrimPrefix(remote, "http://github.com/")
	parts := strings.Split(remote, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fallback
	}
	return parts[0] + "/" + parts[1]
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func isLocalGitPath(remote string) bool {
	return strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, ".")
}

func nullStr(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullStrValue(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
