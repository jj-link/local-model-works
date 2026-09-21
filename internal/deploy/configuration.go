package deploy

import (
	"context"
	"database/sql"
	"errors"
)

// PlanDeploymentConfiguration reviews new launch inputs for the deployment's
// immutable recipe. Repository ownership is neither required nor manufactured.
func (s *Service) PlanDeploymentConfiguration(ctx context.Context, deploymentID string, settings RepositoryReplacementSettings) (*RepositoryUpdatePlan, error) {
	row, err := s.q.GetDeployment(ctx, deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknown
	}
	if err != nil {
		return nil, err
	}
	return s.planReplacement(ctx, "", RepositoryReplacementRequest{
		TargetDigest:       row.RecipeDigest,
		DeploymentIDs:      []string{deploymentID},
		DeploymentSettings: map[string]RepositoryReplacementSettings{deploymentID: settings},
	})
}

// CreateDeploymentConfiguration uses the replacement coordinator, including its
// durable acquisition, placement preservation, cancellation and source rollback.
func (s *Service) CreateDeploymentConfiguration(ctx context.Context, deploymentID string, settings RepositoryReplacementSettings, planDigest string) (string, error) {
	plan, err := s.PlanDeploymentConfiguration(ctx, deploymentID, settings)
	if err != nil {
		return "", err
	}
	return s.createReplacement(ctx, plan, planDigest)
}
