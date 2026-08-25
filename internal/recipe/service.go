// Service: the installed-recipe store with import from catalog, OCI
// reference, pinned Git, and local directories, plus operator trust
// transitions. Trust states:
//
//	local     — imported from a local path or the first-party catalog
//	untrusted — imported from a remote source without a verified signature
//	verified  — a remote import whose package signature validated against
//	            the configured trust key (import-set only, never operator-set)
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
	"github.com/jj-link/local-model-works/internal/sign"
)

// Trust states.
const (
	TrustVerified  = "verified"
	TrustLocal     = "local"
	TrustUntrusted = "untrusted"
)

var (
	ErrUnknown     = errors.New("unknown recipe")
	ErrReference   = errors.New("recipe referenced")
	ErrTrustState  = errors.New("invalid trust state")
	ErrDiffPending = errors.New("permission diff not accepted")
	// ErrUnpinnedRevision — a git source revision is not a full 40-hex
	// commit. Git installs pin an immutable commit; tags and branches move.
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
	DisplayName   string          `json:"display_name,omitempty"`
	Model         string          `json:"model,omitempty"`
	Engine        string          `json:"engine,omitempty"`
	Description   string          `json:"description,omitempty"`
	License       string          `json:"license,omitempty"`
	Source        json.RawMessage `json:"source"`
	TrustState    string          `json:"trust_state"`
	Permissions   []string        `json:"permissions,omitempty"`
	Compatibility json.RawMessage `json:"compatibility,omitempty"`
	ArtifactCount int             `json:"artifact_count"`
	ProfileCount  int             `json:"profile_count"`
	VariantCount  int             `json:"variant_count"`
	HighRisk      []string        `json:"high_risk,omitempty"`
	InstalledAt   string          `json:"installed_at"`
	VersionCount  int             `json:"version_count,omitempty"`
}

// RecipeDetail adds the full manifest (openapi RecipeDetail).
type RecipeDetail struct {
	Recipe
	Manifest json.RawMessage `json:"manifest"`
}

type Service struct {
	db            *sql.DB
	q             *db.Queries
	bus           *events.EventBus
	validator     *Validator
	catalogSchema *jsonschema.Schema
	trustKey      []byte // PEM, empty when unconfigured
	catalogRoot   string
	packageRoot   string
	onInstalled   func()
}

// New builds the store. packageRoot holds complete immutable OCI packages.
func New(sqlDB *sql.DB, q *db.Queries, bus *events.EventBus, v *Validator, trustKeyPEMPath, catalogRoot, packageRoot string) (*Service, error) {
	var keyPEM []byte
	if trustKeyPEMPath != "" {
		b, err := os.ReadFile(trustKeyPEMPath)
		if err != nil {
			return nil, fmt.Errorf("recipe trust key: %w", err)
		}
		keyPEM = b
	}
	if packageRoot == "" {
		return nil, fmt.Errorf("recipe package root is required")
	}
	catalogSchema, err := compileCatalogSchema()
	if err != nil {
		return nil, fmt.Errorf("catalog schema: %w", err)
	}
	service := &Service{
		db: sqlDB, q: q, bus: bus, validator: v, catalogSchema: catalogSchema,
		trustKey: keyPEM, catalogRoot: catalogRoot, packageRoot: packageRoot,
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
func (s *Service) Store(ctx context.Context, doc []byte, source RecipeSource, trustState string) (Recipe, error) {
	res, err := PackManifest(doc, map[string][]byte{}, nil)
	if err != nil {
		return Recipe{}, err
	}
	return s.storePack(ctx, res, source, trustState)
}

func (s *Service) storePack(ctx context.Context, res *PackResult, source RecipeSource, trustState string) (Recipe, error) {
	if trustState != TrustVerified && trustState != TrustLocal && trustState != TrustUntrusted {
		return Recipe{}, fmt.Errorf("%w: %s", ErrTrustState, trustState)
	}
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
			metadata, _ := json.Marshal(map[string]any{"name": artifact.Name, "mount": artifact.Mount, "variant": key})
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
		DisplayName: nullStr(manifest.Metadata.DisplayName), Description: nullStr(manifest.Metadata.Description),
		License: nullStr(manifest.Metadata.License), Source: string(srcJSON),
		TrustState: trustState, Manifest: string(mj),
	}); err != nil {
		return Recipe{}, err
	}
	if err := tx.Commit(); err != nil {
		return Recipe{}, err
	}
	keepPackage = true
	s.bus.Publish(ctx, "recipe.installed", "", mustJSON(map[string]any{
		"digest": digest, "name": manifest.Metadata.Name, "version": manifest.Metadata.Version, "trust_state": trustState,
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
		return s.importDir(ctx, src.Path, src, TrustLocal)
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
			return s.importDir(ctx, dir, kept, TrustVerified)
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
		return Recipe{}, fmt.Errorf("%w: unknown source type %q", ErrTrustState, src.Type)
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

// List returns all installed recipes, most recently installed first.
func (s *Service) List(ctx context.Context) ([]Recipe, error) {
	rows, err := s.q.ListRecipes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Recipe, 0, len(rows))
	for i := range rows {
		var m *Manifest
		if err := json.Unmarshal([]byte(rows[i].Manifest), &m); err != nil {
			return nil, err
		}
		v, err := s.render(ctx, rows[i], m)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// SetTrust applies an operator trust transition. Only local|untrusted are
// operator-settable; verified is import-set. Moving to local requires
// permission_diff_accepted.
func (s *Service) SetTrust(ctx context.Context, digest, state string, diffAccepted bool) (Recipe, error) {
	if state != TrustLocal && state != TrustUntrusted {
		return Recipe{}, fmt.Errorf("%w: only %s and %s are operator-settable", ErrTrustState, TrustLocal, TrustUntrusted)
	}
	row, err := s.q.GetRecipe(ctx, digest)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Recipe{}, ErrUnknown
		}
		return Recipe{}, err
	}
	if row.TrustState == TrustVerified && state == TrustLocal {
		return Recipe{}, fmt.Errorf("%w: a verified recipe cannot be downgraded to local", ErrTrustState)
	}
	if state == TrustLocal && row.TrustState != TrustLocal && !diffAccepted {
		return Recipe{}, fmt.Errorf("%w: review the permission diff first", ErrDiffPending)
	}
	if err := s.q.UpdateRecipeTrust(ctx, db.UpdateRecipeTrustParams{TrustState: state, Digest: digest}); err != nil {
		return Recipe{}, err
	}
	s.bus.Publish(ctx, "recipe.trust", "", mustJSON(map[string]any{"digest": digest, "trust_state": state}))
	var m *Manifest
	if err := json.Unmarshal([]byte(row.Manifest), &m); err != nil {
		return Recipe{}, err
	}
	row.TrustState = state
	return s.render(ctx, row, m)
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
func (s *Service) importDir(ctx context.Context, dir string, src RecipeSource, trust string) (Recipe, error) {
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
		return s.storePack(ctx, res, src, trust)
	}
	_, res, err := PackFromDir(dir, s.validator)
	if err != nil {
		return Recipe{}, err
	}
	return s.storePack(ctx, res, src, trust)
}

// importOCI pulls one immutable package and verifies any key-signed Sigstore
// referrer without contacting Fulcio or Rekor.
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
	trust, err := s.verifyOCIReferrers(ctx, repo, manifestDesc, manifestBytes)
	if err != nil {
		return Recipe{}, err
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
	return s.storePack(ctx, res, kept, trust)
}

func (s *Service) verifyOCIReferrers(ctx context.Context, repo *remote.Repository, subject ocispec.Descriptor, artifact []byte) (string, error) {
	var referrers []ocispec.Descriptor
	if err := repo.Referrers(ctx, subject, SigstoreBundleArtifactType, func(page []ocispec.Descriptor) error {
		if len(referrers)+len(page) > 8 {
			return fmt.Errorf("too many signature referrers")
		}
		referrers = append(referrers, page...)
		return nil
	}); err != nil {
		return "", fmt.Errorf("list signature referrers: %w", err)
	}
	if len(referrers) == 0 {
		return TrustUntrusted, nil
	}
	if len(s.trustKey) == 0 {
		return "", fmt.Errorf("package is signed but no trust key is configured")
	}
	var verificationErr error
	for _, descriptor := range referrers {
		raw, err := fetchDesc(ctx, repo, descriptor)
		if err != nil {
			verificationErr = err
			continue
		}
		var referrer ociManifest
		if err := json.Unmarshal(raw, &referrer); err != nil {
			verificationErr = err
			continue
		}
		if referrer.ArtifactType != SigstoreBundleArtifactType || referrer.Subject == nil ||
			referrer.Subject.Digest != subject.Digest.String() ||
			referrer.Config.MediaType != SigstoreBundleArtifactType || len(referrer.Layers) != 0 {
			verificationErr = fmt.Errorf("invalid Sigstore referrer manifest")
			continue
		}
		if !sha256DigestRE.MatchString(referrer.Config.Digest) {
			verificationErr = fmt.Errorf("invalid Sigstore bundle digest")
			continue
		}
		bundleJSON, err := fetchDesc(ctx, repo, toSpecDesc(referrer.Config))
		if err != nil {
			verificationErr = err
			continue
		}
		if err := sign.VerifyBundle(bundleJSON, artifact, s.trustKey); err != nil {
			verificationErr = err
			continue
		}
		return TrustVerified, nil
	}
	if verificationErr == nil {
		verificationErr = fmt.Errorf("no valid Sigstore bundle found")
	}
	return "", fmt.Errorf("signature: %w", verificationErr)
}

// importGit clones one exact commit into an ephemeral checkout. Only the
// validated package survives; Git imports remain untrusted until review.
func (s *Service) importGit(ctx context.Context, src RecipeSource) (Recipe, error) {
	if src.Remote == "" {
		return Recipe{}, fmt.Errorf("git source: remote is required")
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
	return s.importDir(ctx, target, kept, TrustUntrusted)
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
	versions, err := s.q.ListRecipeVersions(ctx, m.Metadata.Name)
	if err != nil {
		return Recipe{}, err
	}
	v := Recipe{
		Digest:        row.Digest,
		Name:          row.Name,
		Version:       row.Version,
		DisplayName:   nullStrValue(row.DisplayName),
		Model:         m.Metadata.Model,
		Engine:        m.Metadata.Engine,
		Description:   nullStrValue(row.Description),
		License:       nullStrValue(row.License),
		Source:        json.RawMessage(row.Source),
		TrustState:    row.TrustState,
		ArtifactCount: len(m.Artifacts),
		HighRisk:      m.HighRiskPermissions(),
		InstalledAt:   row.InstalledAt,
		VersionCount:  len(versions),
	}
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
