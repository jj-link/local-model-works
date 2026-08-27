package recipe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// Repository is the logical recipe identity shown by the Library. Commits and
// package digests remain immutable versions beneath it.
type Repository struct {
	ID                 string              `json:"id"`
	SourceURL          string              `json:"source_url"`
	SourcePath         string              `json:"source_path"`
	TrackingRef        string              `json:"tracking_ref"`
	Current            *Recipe             `json:"current_recipe,omitempty"`
	InstalledCommit    string              `json:"installed_commit,omitempty"`
	ObservedHeadCommit string              `json:"observed_head_commit,omitempty"`
	ObservedHeadTree   string              `json:"observed_head_tree,omitempty"`
	HeadCheckedAt      string              `json:"head_checked_at,omitempty"`
	UpdateAvailable    bool                `json:"update_available"`
	UpdateSupported    bool                `json:"update_supported"`
	UpdateDiagnostic   string              `json:"update_diagnostic,omitempty"`
	Versions           []RepositoryVersion `json:"versions"`
	CreatedAt          string              `json:"created_at"`
	UpdatedAt          string              `json:"updated_at"`
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
	for _, versionRow := range versionRows {
		manifest := &Manifest{}
		if err := json.Unmarshal([]byte(versionRow.Manifest), manifest); err != nil {
			return Repository{}, fmt.Errorf("recipe manifest %s: %w", versionRow.RecipeDigest, err)
		}
		recipeRow := db.Recipe{
			Digest:      versionRow.RecipeDigest,
			Name:        versionRow.Name,
			Version:     versionRow.Version,
			DisplayName: versionRow.DisplayName,
			Description: versionRow.Description,
			License:     versionRow.License,
			Source:      versionRow.Source,
			TrustState:  versionRow.TrustState,
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

func attachRepositoryVersion(ctx context.Context, q *db.Queries, manifest *Manifest, digest, treeSHA, installedAt string) error {
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
