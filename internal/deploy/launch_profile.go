package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// Saved launch profile: an operator-named combination of artifact variants
// and parameter overrides pinned to one recipe digest.
type LaunchProfile struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	RecipeDigest string            `json:"recipe_digest"`
	Variants     map[string]string `json:"variants,omitempty"`
	Parameters   map[string]any    `json:"parameters,omitempty"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
}

// UpsertLaunchProfileRequest creates or renames a saved profile. Variants
// and parameters repeat on update; the recipe digest is immutable.
type UpsertLaunchProfileRequest struct {
	Name         string            `json:"name"`
	RecipeDigest string            `json:"recipe_digest"`
	Variants     map[string]string `json:"variants,omitempty"`
	Parameters   map[string]any    `json:"parameters,omitempty"`
}

func rowToLaunchProfile(r db.LaunchProfile) (*LaunchProfile, error) {
	p := &LaunchProfile{
		ID: r.ID, Name: r.Name, RecipeDigest: r.RecipeDigest,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if err := json.Unmarshal([]byte(r.Variants), &p.Variants); err != nil && r.Variants != "" && r.Variants != "{}" {
		return nil, fmt.Errorf("profile variants: %w", err)
	}
	if err := json.Unmarshal([]byte(r.Parameters), &p.Parameters); err != nil && r.Parameters != "" && r.Parameters != "{}" {
		return nil, fmt.Errorf("profile parameters: %w", err)
	}
	if p.Variants == nil {
		p.Variants = map[string]string{}
	}
	if p.Parameters == nil {
		p.Parameters = map[string]any{}
	}
	return p, nil
}

func marshalJSONOrEmpty(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func effectiveLaunchValues(m *recipe.Manifest, variants map[string]string, parameters map[string]any) (map[string]string, map[string]any, error) {
	table := m.VariantTable()
	for name, value := range variants {
		declared, ok := table[name]
		if !ok || len(declared) == 0 || !containsVariant(declared, value) {
			return nil, nil, fmt.Errorf("artifact %q has no variant %q", name, value)
		}
	}
	resolvedVariants := map[string]string{}
	for i := range m.Artifacts {
		artifact := &m.Artifacts[i]
		if len(artifact.Variants) == 0 {
			continue
		}
		value := variants[artifact.Name]
		if value == "" {
			value = artifact.DefaultVariant
		}
		if !containsVariant(table[artifact.Name], value) {
			return nil, nil, fmt.Errorf("artifact %q has no variant %q", artifact.Name, value)
		}
		resolvedVariants[artifact.Name] = value
	}
	resolvedParameters, err := m.EffectiveSettings(parameters)
	if err != nil {
		return nil, nil, err
	}
	return resolvedVariants, resolvedParameters, nil
}

func (s *Service) validateLaunchProfileValues(ctx context.Context, digest string, variants map[string]string, parameters map[string]any) error {
	row, err := s.q.GetRecipe(ctx, digest)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRecipe, digest)
	}
	m, err := recipe.Parse([]byte(row.Manifest))
	if err != nil {
		return fmt.Errorf("recipe manifest: %w", err)
	}
	if _, _, err := effectiveLaunchValues(m, variants, parameters); err != nil {
		return fmt.Errorf("%w: %v", ErrProfile, err)
	}
	return nil
}

func containsVariant(list []string, value string) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}

// CreateLaunchProfile saves a named, validated combination of variants and
// parameter overrides for one recipe digest.
func (s *Service) CreateLaunchProfile(ctx context.Context, req UpsertLaunchProfileRequest) (*LaunchProfile, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrProfile)
	}
	if req.RecipeDigest == "" {
		return nil, fmt.Errorf("%w: recipe_digest is required", ErrProfile)
	}
	if err := s.validateLaunchProfileValues(ctx, req.RecipeDigest, req.Variants, req.Parameters); err != nil {
		return nil, err
	}
	profileID, _ := id.New()
	if err := s.q.CreateLaunchProfile(ctx, db.CreateLaunchProfileParams{
		ID: profileID, Name: req.Name, RecipeDigest: req.RecipeDigest,
		Variants:   marshalJSONOrEmpty(req.Variants),
		Parameters: marshalJSONOrEmpty(req.Parameters),
	}); err != nil {
		return nil, err
	}
	return s.GetLaunchProfile(ctx, profileID)
}

// UpdateLaunchProfile renames a profile and replaces its saved values. The
// pinned digest is immutable; superseding a repository update requires
// deleting and re-saving against the new digest.
func (s *Service) UpdateLaunchProfile(ctx context.Context, profileID string, req UpsertLaunchProfileRequest) (*LaunchProfile, error) {
	existing, err := s.q.GetLaunchProfile(ctx, profileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnknown
		}
		return nil, err
	}
	if req.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrProfile)
	}
	if req.RecipeDigest != "" && req.RecipeDigest != existing.RecipeDigest {
		return nil, fmt.Errorf("%w: recipe digest is pinned", ErrProfile)
	}
	if err := s.validateLaunchProfileValues(ctx, existing.RecipeDigest, req.Variants, req.Parameters); err != nil {
		return nil, err
	}
	if err := s.q.UpdateLaunchProfile(ctx, db.UpdateLaunchProfileParams{
		Name: req.Name, Variants: marshalJSONOrEmpty(req.Variants),
		Parameters: marshalJSONOrEmpty(req.Parameters), ID: profileID,
	}); err != nil {
		return nil, err
	}
	return s.GetLaunchProfile(ctx, profileID)
}

func (s *Service) DeleteLaunchProfile(ctx context.Context, profileID string) error {
	if _, err := s.q.GetLaunchProfile(ctx, profileID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknown
		}
		return err
	}
	return s.q.DeleteLaunchProfile(ctx, profileID)
}

func (s *Service) GetLaunchProfile(ctx context.Context, profileID string) (*LaunchProfile, error) {
	row, err := s.q.GetLaunchProfile(ctx, profileID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnknown
		}
		return nil, err
	}
	return rowToLaunchProfile(row)
}

func (s *Service) ListLaunchProfiles(ctx context.Context, digest string) ([]*LaunchProfile, error) {
	rows, err := s.q.ListLaunchProfilesByRecipe(ctx, digest)
	if err != nil {
		return nil, err
	}
	out := make([]*LaunchProfile, 0, len(rows))
	for _, r := range rows {
		p, err := rowToLaunchProfile(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// resolveSettings computes resolved artifact variants and parameter values
// for a plan. Saved profiles are digest-pinned and revalidated at plan time.
func resolveSettings(ctx context.Context, s *Service, m *recipe.Manifest, digest string, req PlanRequest) (map[string]string, map[string]any, error) {
	var variants map[string]string
	var parameters map[string]any
	if req.LaunchProfileID != "" {
		if len(req.Variants) > 0 || len(req.Parameters) > 0 {
			return nil, nil, fmt.Errorf("%w: launch_profile_id is mutually exclusive with variants/parameters", ErrProfile)
		}
		row, err := s.q.GetLaunchProfile(ctx, req.LaunchProfileID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("%w: launch profile %s", ErrProfile, req.LaunchProfileID)
			}
			return nil, nil, err
		}
		if row.RecipeDigest != digest {
			return nil, nil, fmt.Errorf("%w: launch profile %q is pinned to a different recipe digest", ErrProfile, row.Name)
		}
		if err := json.Unmarshal([]byte(row.Variants), &variants); err != nil && row.Variants != "" && row.Variants != "{}" {
			return nil, nil, fmt.Errorf("%w: profile variants: %v", ErrProfile, err)
		}
		if err := json.Unmarshal([]byte(row.Parameters), &parameters); err != nil && row.Parameters != "" && row.Parameters != "{}" {
			return nil, nil, fmt.Errorf("%w: profile parameters: %v", ErrProfile, err)
		}
	} else {
		variants = req.Variants
		parameters = req.Parameters
	}
	resolvedVariants, resolvedParameters, err := effectiveLaunchValues(m, variants, parameters)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrProfile, err)
	}
	return resolvedVariants, resolvedParameters, nil
}
