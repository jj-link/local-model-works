package recipe

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// ImportReviewed saves a reviewed immutable package without acquiring device
// resources or changing workloads. Empty repositoryID creates a new addition;
// an existing repository requires its reviewed current digest.
func (s *Service) ImportReviewed(ctx context.Context, src RecipeSource, repositoryID, expectedCurrentDigest string) (Recipe, error) {
	if src.Type != "local" {
		return Recipe{}, fmt.Errorf("reviewed import requires a materialized local package")
	}
	if repositoryID == "" && expectedCurrentDigest != "" {
		return Recipe{}, fmt.Errorf("expected digest requires a repository")
	}
	packed, err := ReadLayout(src.Path)
	if err != nil {
		return Recipe{}, err
	}
	return s.storeReviewedPack(ctx, packed, src, &reviewedSelection{repositoryID: repositoryID, expectedDigest: expectedCurrentDigest})
}

// ReadPackage returns the verified immutable package for a saved digest, never
// a reconstructed manifest or a mutable source checkout.
func (s *Service) ReadPackage(ctx context.Context, digest string) (*PackResult, error) {
	if _, err := s.Get(ctx, digest); err != nil {
		return nil, err
	}
	packed, err := ReadLayout(filepath.Join(s.packageRoot, strings.TrimPrefix(digest, "sha256:")))
	if err != nil {
		return nil, err
	}
	if packed.ManifestDigest != digest {
		return nil, fmt.Errorf("saved recipe package digest mismatch")
	}
	return packed, nil
}
