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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"

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

// SignatureMediaType is the OCI layer carrying the detached signature.
const SignatureMediaType = "application/vnd.localmodelworks.recipe.signature.v1+json"

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

// Service is the installed-recipe store.
type Service struct {
	q           *db.Queries
	bus         *events.EventBus
	validator   *Validator
	trustKey    []byte // PEM, empty when unconfigured
	catalogRoot string
}

// New builds the store. trustKeyPEMPath is the operator-configured public
// key (PEM PKIX, Ed25519) used to verify remote package signatures; empty
// disables verification. catalogRoot is the on-disk first-party catalog.
func New(q *db.Queries, bus *events.EventBus, v *Validator, trustKeyPEMPath, catalogRoot string) (*Service, error) {
	var keyPEM []byte
	if trustKeyPEMPath != "" {
		b, err := os.ReadFile(trustKeyPEMPath)
		if err != nil {
			return nil, fmt.Errorf("recipe trust key: %w", err)
		}
		keyPEM = b
	}
	return &Service{q: q, bus: bus, validator: v, trustKey: keyPEM, catalogRoot: catalogRoot}, nil
}

// Store validates, canonicalizes, and inserts one recipe document.
// Re-storing the same digest is a no-op returning the stored row.
func (s *Service) Store(ctx context.Context, doc []byte, source RecipeSource, trustState string) (Recipe, error) {
	if trustState != TrustVerified && trustState != TrustLocal && trustState != TrustUntrusted {
		return Recipe{}, fmt.Errorf("%w: %s", ErrTrustState, trustState)
	}
	canon, err := Canonical(doc)
	if err != nil {
		return Recipe{}, fmt.Errorf("canonicalize: %w", err)
	}
	manifest, vds, err := s.validator.ValidateStrict(canon)
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
	digest, err := DigestOf(canon)
	if err != nil {
		return Recipe{}, err
	}
	row, err := s.q.GetRecipe(ctx, digest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Recipe{}, err
	}
	if err == nil {
		return s.render(ctx, row, manifest)
	}
	srcJSON, err := json.Marshal(source)
	if err != nil {
		return Recipe{}, err
	}
	mj, err := json.Marshal(manifest)
	if err != nil {
		return Recipe{}, err
	}
	if err := s.q.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: manifest.Metadata.Name, Version: manifest.Metadata.Version,
		DisplayName: nullStr(manifest.Metadata.DisplayName), Description: nullStr(manifest.Metadata.Description),
		License: nullStr(manifest.Metadata.License), Source: string(srcJSON),
		TrustState: trustState, Manifest: string(mj),
	}); err != nil {
		return Recipe{}, err
	}
	s.bus.Publish(ctx, "recipe.installed", "", mustJSON(map[string]any{
		"digest": digest, "name": manifest.Metadata.Name, "version": manifest.Metadata.Version, "trust_state": trustState,
	}))
	detail, err := s.Get(ctx, digest)
	if err != nil {
		return Recipe{}, err
	}
	return detail.Recipe, nil
}

// Import dispatches a RecipeSource.
func (s *Service) Import(ctx context.Context, src RecipeSource) (Recipe, error) {
	switch src.Type {
	case "local":
		return s.importDir(ctx, src.Path, src, TrustLocal)
	case "catalog":
		if s.catalogRoot == "" {
			return Recipe{}, fmt.Errorf("%w: catalog is not configured", ErrUnknown)
		}
		dir, err := s.catalogEntry(src.Reference)
		if err != nil {
			return Recipe{}, err
		}
		kept := src
		kept.Reference = ""
		kept.Path = dir
		return s.importDir(ctx, dir, kept, TrustLocal)
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
	deps, err := s.q.RecipeReferencedByDeployments(ctx, digest)
	if err != nil {
		return err
	}
	if deps > 0 {
		return fmt.Errorf("%w: %d deployment(s) reference this recipe", ErrReference, deps)
	}
	runs, err := s.q.RecipeReferencedByRuns(ctx, digest)
	if err != nil {
		return err
	}
	if runs > 0 {
		return fmt.Errorf("%w: %d run(s) reference this recipe", ErrReference, runs)
	}
	if err := s.q.DeleteRecipe(ctx, digest); err != nil {
		return err
	}
	s.bus.Publish(ctx, "recipe.removed", "", mustJSON(map[string]any{"digest": digest}))
	return nil
}

// importDir loads a recipe from a directory (plain recipe directory or
// on-disk OCI layout) and stores it under the given trust state.
func (s *Service) importDir(ctx context.Context, dir string, src RecipeSource, trust string) (Recipe, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Recipe{}, fmt.Errorf("source directory: %w", err)
	}
	if !info.IsDir() {
		return Recipe{}, fmt.Errorf("source directory: %s is not a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		doc, err := readLayoutConfig(dir)
		if err != nil {
			return Recipe{}, fmt.Errorf("OCI layout: %w", err)
		}
		return s.Store(ctx, doc, src, trust)
	}
	_, res, err := PackFromDir(dir, s.validator)
	if err != nil {
		return Recipe{}, err
	}
	return s.Store(ctx, res.ConfigJSON, src, trust)
}

// readLayoutConfig verifies an on-disk OCI layout and returns its
// config blob (the canonical recipe document).
func readLayoutConfig(dir string) ([]byte, error) {
	if err := VerifyLayout(dir); err != nil {
		return nil, err
	}
	idx, err := os.ReadFile(path.Join(dir, "index.json"))
	if err != nil {
		return nil, err
	}
	var ix ociIndex
	if err := json.Unmarshal(idx, &ix); err != nil {
		return nil, err
	}
	if len(ix.Blobs) == 0 {
		return nil, fmt.Errorf("layout index has no entries")
	}
	manifestPath := path.Join("blobs", "sha256", ix.Blobs[0].Digest[strings.LastIndexByte(ix.Blobs[0].Digest, ':')+1:])
	mb, err := os.ReadFile(path.Join(dir, manifestPath))
	if err != nil {
		return nil, err
	}
	var m ociManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, err
	}
	if m.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("artifact type %q is not a recipe package", m.ArtifactType)
	}
	if m.Config.MediaType != ConfigMediaType {
		return nil, fmt.Errorf("config media type %q is not a recipe config", m.Config.MediaType)
	}
	configPath := path.Join("blobs", "sha256", m.Config.Digest[strings.LastIndexByte(m.Config.Digest, ':')+1:])
	return os.ReadFile(path.Join(dir, configPath))
}

// importOCI pulls a package from a registry. Signature verification runs
// when a trust key is configured: a present-but-failing signature fails
// the import; a missing signature leaves the recipe untrusted.
func (s *Service) importOCI(ctx context.Context, src RecipeSource) (Recipe, error) {
	if src.Reference == "" {
		return Recipe{}, fmt.Errorf("oci source: reference is required")
	}
	ref, err := registry.ParseReference(src.Reference)
	if err != nil {
		return Recipe{}, fmt.Errorf("oci reference: %w", err)
	}
	repo, err := remote.NewRepository(ref.String())
	if err != nil {
		return Recipe{}, fmt.Errorf("oci reference: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	manifestDesc, err := repo.Resolve(ctx, ref.String())
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
	if m.Config.MediaType != ConfigMediaType {
		return Recipe{}, fmt.Errorf("config media type %q is not a recipe config", m.Config.MediaType)
	}
	doc, err := fetchDesc(ctx, repo, toSpecDesc(m.Config))
	if err != nil {
		return Recipe{}, fmt.Errorf("fetch config: %w", err)
	}
	trust := TrustUntrusted
	for _, l := range m.Layers {
		if l.MediaType != SignatureMediaType {
			continue
		}
		sig, err := fetchDesc(ctx, repo, toSpecDesc(l))
		if err != nil {
			return Recipe{}, fmt.Errorf("fetch signature: %w", err)
		}
		if len(s.trustKey) == 0 {
			return Recipe{}, fmt.Errorf("package is signed but no trust key is configured")
		}
		if err := sign.VerifyEd25519(sig, doc, s.trustKey); err != nil {
			return Recipe{}, fmt.Errorf("signature: %w", err)
		}
		trust = TrustVerified
	}
	kept := src
	kept.Reference = ref.String() + "@" + manifestDesc.Digest.String()
	return s.Store(ctx, doc, kept, trust)
}

// importGit clones the pinned 40-hex commit and loads the recipe at
// subpath. The revision must be a full commit (tags and branches move, so
// they are rejected with ErrUnpinnedRevision); the resolved commit is
// recorded as revision/tree. Git imports are untrusted: the transport
// carries no content signature.
func (s *Service) importGit(ctx context.Context, src RecipeSource) (Recipe, error) {
	if src.Remote == "" {
		return Recipe{}, fmt.Errorf("git source: remote is required")
	}
	if src.Revision == "" {
		return Recipe{}, fmt.Errorf("git source: revision is required")
	}
	if !sha40.MatchString(src.Revision) {
		return Recipe{}, fmt.Errorf("%w: %q is not a 40-hex commit", ErrUnpinnedRevision, src.Revision)
	}
	tmp, err := os.MkdirTemp("", "lmw-recipe-git-*")
	if err != nil {
		return Recipe{}, err
	}
	// A full commit is required, so a plain clone suffices for every
	// remote form (file://, ssh://, https://); no --branch/--depth.
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
	target := tmp
	if src.Path != "" {
		target = filepath.Join(tmp, filepath.Clean(src.Path))
		if !strings.HasPrefix(target, tmp+string(filepath.Separator)) {
			return Recipe{}, fmt.Errorf("git source: path %q escapes the checkout", src.Path)
		}
	}
	kept := src
	kept.Revision = commit
	kept.Tree = commit
	return s.importDir(ctx, target, kept, TrustUntrusted)
}

// catalogEntry resolves a catalog reference "<name>" or "<name>:<version>"
// to a directory under catalogRoot.
func (s *Service) catalogEntry(reference string) (string, error) {
	name, version := reference, ""
	if i := strings.LastIndexByte(reference, ':'); i > 0 && !strings.ContainsAny(reference[:i], "/\\") {
		name, version = reference[:i], reference[i+1:]
	}

	if version != "" {
		dir := filepath.Join(s.catalogRoot, name, version)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
	}
	dir := filepath.Join(s.catalogRoot, name)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: catalog has no entry %q", ErrUnknown, reference)
	}
	return dir, nil
}

// fetchDesc fetches one blob by descriptor.
func fetchDesc(ctx context.Context, repo *remote.Repository, d ocispec.Descriptor) ([]byte, error) {
	r, err := repo.Fetch(ctx, d)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
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
