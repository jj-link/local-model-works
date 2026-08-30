package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

const RepositoryUnsupportedCode = "recipe.repository_unsupported"

// RepositoryVersion links an immutable recipe package to one exact repository
// commit. Non-canonical links retain duplicate historical packages.
type RepositoryVersion struct {
	Recipe      Recipe `json:"recipe"`
	CommitSHA   string `json:"commit_sha"`
	TreeSHA     string `json:"tree_sha,omitempty"`
	Canonical   bool   `json:"canonical"`
	InstalledAt string `json:"installed_at"`
}

// RepositoryInstalledDevice is one node with a valid package placement for
// at least one immutable version of a repository.
type RepositoryInstalledDevice struct {
	NodeID           string   `json:"node_id"`
	NodeName         string   `json:"node_name"`
	NodeStatus       string   `json:"node_status"`
	InstalledDigests []string `json:"installed_digests"`
}

// Repository is the logical recipe identity shown by the Library. Commits and
// package digests remain immutable versions beneath it.
type Repository struct {
	ID                 string                      `json:"id"`
	SourceURL          string                      `json:"source_url"`
	SourcePath         string                      `json:"source_path"`
	TrackingRef        string                      `json:"tracking_ref"`
	Current            *Recipe                     `json:"current_recipe,omitempty"`
	InstalledCommit    string                      `json:"installed_commit,omitempty"`
	ObservedHeadCommit string                      `json:"observed_head_commit,omitempty"`
	ObservedHeadTree   string                      `json:"observed_head_tree,omitempty"`
	HeadCheckedAt      string                      `json:"head_checked_at,omitempty"`
	UpdateAvailable    bool                        `json:"update_available"`
	UpdateSupported    bool                        `json:"update_supported"`
	UpdateDiagnostic   string                      `json:"update_diagnostic,omitempty"`
	Versions           []RepositoryVersion         `json:"versions"`
	InstalledDevices   []RepositoryInstalledDevice `json:"installed_devices"`
	CreatedAt          string                      `json:"created_at"`
	UpdatedAt          string                      `json:"updated_at"`
}

// ListRepositories returns one aggregate per normalized source URL and path.
func (s *Service) ListRepositories(ctx context.Context) ([]Repository, error) {
	rows, err := s.q.ListRecipeRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Repository, 0, len(rows))
	for _, row := range rows {
		repository, err := s.renderRepository(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, repository)
	}
	return out, nil
}

// GetRepository returns one logical recipe aggregate.
func (s *Service) GetRepository(ctx context.Context, id string) (Repository, error) {
	row, err := s.q.GetRecipeRepository(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Repository{}, ErrUnknown
		}
		return Repository{}, err
	}
	return s.renderRepository(ctx, row)
}

func (s *Service) renderRepository(ctx context.Context, row db.RecipeRepository) (Repository, error) {
	versionRows, err := s.q.ListRecipeRepositoryVersions(ctx, row.ID)
	if err != nil {
		return Repository{}, err
	}
	repository := Repository{
		ID:                 row.ID,
		SourceURL:          row.SourceUrl,
		SourcePath:         row.SourcePath,
		TrackingRef:        row.TrackingRef,
		ObservedHeadCommit: nullStrValue(row.ObservedHeadCommit),
		ObservedHeadTree:   nullStrValue(row.ObservedHeadTree),
		HeadCheckedAt:      nullStrValue(row.HeadCheckedAt),
		Versions:           make([]RepositoryVersion, 0, len(versionRows)),
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
		UpdateDiagnostic:   RepositoryUnsupportedCode,
	}
	digests := make([]string, 0, len(versionRows))
	for _, versionRow := range versionRows {
		digests = append(digests, versionRow.RecipeDigest)
	}
	repository.InstalledDevices, err = repositoryInstalledDevicesForDigests(ctx, s.q, digests)
	if err != nil {
		return Repository{}, err
	}
	for _, versionRow := range versionRows {
		manifest := &Manifest{}
		if err := json.Unmarshal([]byte(versionRow.Manifest), manifest); err != nil {
			return Repository{}, fmt.Errorf("recipe manifest %s: %w", versionRow.RecipeDigest, err)
		}
		recipeRow := db.Recipe{
			Digest:      versionRow.RecipeDigest,
			Name:        versionRow.Name,
			Version:     versionRow.Version,
			Description: versionRow.Description,
			License:     versionRow.License,
			Source:      versionRow.Source,
			Manifest:    versionRow.Manifest,
			InstalledAt: versionRow.InstalledAt,
		}
		rendered, err := s.render(ctx, recipeRow, manifest)
		if err != nil {
			return Repository{}, err
		}
		rendered.VersionCount = len(versionRows)
		version := RepositoryVersion{
			Recipe:      rendered,
			CommitSHA:   versionRow.CommitSha,
			TreeSHA:     nullStrValue(versionRow.TreeSha),
			Canonical:   versionRow.Canonical == 1,
			InstalledAt: versionRow.InstalledAt,
		}
		repository.Versions = append(repository.Versions, version)
		if row.CurrentDigest.Valid && row.CurrentDigest.String == versionRow.RecipeDigest {
			current := rendered
			repository.Current = &current
			repository.InstalledCommit = versionRow.CommitSha
		}
	}
	if supporter, ok := s.repositoryCompilers.(interface{ SupportsRepository(string) bool }); ok {
		repository.UpdateSupported = supporter.SupportsRepository(repository.ID)
	}
	if repository.Current != nil {
		var installedSource RecipeSource
		if json.Unmarshal(repository.Current.Source, &installedSource) == nil && installedSource.Type == "git" {
			repository.UpdateSupported = true
		}
	}
	if repository.UpdateSupported {
		repository.UpdateDiagnostic = ""
	}
	repository.UpdateAvailable = repository.ObservedHeadCommit != "" && !strings.EqualFold(repository.ObservedHeadCommit, repository.InstalledCommit)
	return repository, nil
}

// ListRepositoryInstalledDevices returns valid package placements for every
// immutable version linked to one repository.
func ListRepositoryInstalledDevices(ctx context.Context, q *db.Queries, repositoryID string) ([]RepositoryInstalledDevice, error) {
	versions, err := q.ListRecipeRepositoryVersions(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(versions))
	for _, version := range versions {
		digests = append(digests, version.RecipeDigest)
	}
	return repositoryInstalledDevicesForDigests(ctx, q, digests)
}

func repositoryInstalledDevicesForDigests(ctx context.Context, q *db.Queries, digests []string) ([]RepositoryInstalledDevice, error) {
	byNode := map[string]*RepositoryInstalledDevice{}
	for _, digest := range digests {
		artifact, err := q.GetArtifactByIdentity(ctx, "recipe://"+digest)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		placements, err := q.ListPlacements(ctx, artifact.ID)
		if err != nil {
			return nil, err
		}
		for _, placement := range placements {
			if placement.State != "valid" {
				continue
			}
			device := byNode[placement.NodeID]
			if device == nil {
				node, err := q.GetNode(ctx, placement.NodeID)
				if err != nil {
					return nil, err
				}
				device = &RepositoryInstalledDevice{
					NodeID: placement.NodeID, NodeName: node.DisplayName, NodeStatus: node.Status,
					InstalledDigests: []string{},
				}
				byNode[placement.NodeID] = device
			}
			if !containsString(device.InstalledDigests, digest) {
				device.InstalledDigests = append(device.InstalledDigests, digest)
			}
		}
	}
	devices := make([]RepositoryInstalledDevice, 0, len(byNode))
	for _, device := range byNode {
		sort.Strings(device.InstalledDigests)
		devices = append(devices, *device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].NodeID < devices[j].NodeID })
	return devices, nil
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func attachRepositoryVersion(ctx context.Context, q *db.Queries, manifest *Manifest, digest, treeSHA, installedAt string) error {
	return attachRepositoryVersionWithCurrent(ctx, q, manifest, digest, treeSHA, installedAt, true)
}

func attachRepositoryVersionWithCurrent(ctx context.Context, q *db.Queries, manifest *Manifest, digest, treeSHA, installedAt string, setCurrent bool) error {
	if manifest.Metadata.Source == nil || manifest.Metadata.Source.URL == "" || manifest.Metadata.Source.Revision == "" {
		return nil
	}
	id, sourceURL, sourcePath, err := RepositoryIdentity(*manifest.Metadata.Source)
	if err != nil {
		return err
	}
	commitSHA := strings.ToLower(manifest.Metadata.Source.Revision)
	if !sha40.MatchString(commitSHA) {
		return fmt.Errorf("recipe repository: revision %q is not a 40-hex commit", manifest.Metadata.Source.Revision)
	}
	if installedAt == "" {
		installedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := q.UpsertRecipeRepository(ctx, db.UpsertRecipeRepositoryParams{
		ID: id, SourceUrl: sourceURL, SourcePath: sourcePath, TrackingRef: "HEAD",
		CreatedAt: installedAt, UpdatedAt: now,
	}); err != nil {
		return err
	}
	if err := q.ClearCanonicalRecipeRepositoryCommit(ctx, db.ClearCanonicalRecipeRepositoryCommitParams{
		RepositoryID: id, CommitSha: commitSHA,
	}); err != nil {
		return err
	}
	if err := q.AttachRecipeRepositoryVersion(ctx, db.AttachRecipeRepositoryVersionParams{
		RepositoryID: id, RecipeDigest: digest, CommitSha: commitSHA,
		TreeSha: nullableString(treeSHA), Canonical: 1, InstalledAt: installedAt,
	}); err != nil {
		return err
	}
	if !setCurrent {
		return nil
	}
	return q.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
		CurrentDigest: nullableString(digest), UpdatedAt: now, ID: id,
	})
}

func detachRepositoryVersion(ctx context.Context, q *db.Queries, digest string) error {
	link, err := q.GetRecipeRepositoryVersionByDigest(ctx, digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	repository, err := q.GetRecipeRepository(ctx, link.RepositoryID)
	if err != nil {
		return err
	}
	wasCurrent := repository.CurrentDigest.Valid && repository.CurrentDigest.String == digest
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if wasCurrent {
		if err := q.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
			CurrentDigest: sql.NullString{}, UpdatedAt: now, ID: repository.ID,
		}); err != nil {
			return err
		}
	}
	if err := q.DeleteRecipeRepositoryVersionByDigest(ctx, digest); err != nil {
		return err
	}
	remaining, err := q.ListRecipeRepositoryVersions(ctx, repository.ID)
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		return q.DeleteRecipeRepository(ctx, repository.ID)
	}
	hasCanonicalCommit := false
	for _, version := range remaining {
		if version.CommitSha == link.CommitSha && version.Canonical == 1 {
			hasCanonicalCommit = true
			break
		}
	}
	if !hasCanonicalCommit {
		for _, version := range remaining {
			if version.CommitSha == link.CommitSha {
				if err := q.SetRecipeRepositoryVersionCanonical(ctx, db.SetRecipeRepositoryVersionCanonicalParams{
					RepositoryID: repository.ID, RecipeDigest: version.RecipeDigest,
				}); err != nil {
					return err
				}
				break
			}
		}
	}
	if wasCurrent {
		nextDigest := remaining[0].RecipeDigest
		for _, version := range remaining {
			if version.Canonical == 1 {
				nextDigest = version.RecipeDigest
				break
			}
		}
		return q.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
			CurrentDigest: nullableString(nextDigest), UpdatedAt: now, ID: repository.ID,
		})
	}
	return nil
}
