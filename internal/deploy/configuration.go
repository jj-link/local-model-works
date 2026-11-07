package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/runs"
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

// CreateDeploymentConfiguration changes the deployment's own settings. It is
// the same deployment before and after: a running one is stopped, given the
// reviewed settings and started again; a stopped one only has them saved.
// Nothing else is created, and a failed start leaves it stopped, not restored.
func (s *Service) CreateDeploymentConfiguration(ctx context.Context, deploymentID string, settings RepositoryReplacementSettings, planDigest string) (string, error) {
	plan, err := s.PlanDeploymentConfiguration(ctx, deploymentID, settings)
	if err != nil {
		return "", err
	}
	if plan.Digest != planDigest {
		return "", fmt.Errorf("%w: %s != %s", ErrPlanStale, planDigest, plan.Digest)
	}
	if !plan.Ready || len(plan.Deployments) != 1 {
		return "", fmt.Errorf("%w: configuration plan is not ready", ErrNotReady)
	}
	row, err := s.q.GetDeployment(ctx, deploymentID)
	if err != nil {
		return "", err
	}
	if row.DesiredState != "running" && row.ObservedState != "stopped" {
		return "", fmt.Errorf("%w: deployment is %s", ErrState, row.ObservedState)
	}
	runID, err := s.runs.Create(ctx, "serving", "configure", map[string]any{
		"deployment_id": deploymentID, "parameters": plan.Deployments[0].DeploymentPlan.Parameters, "plan_digest": planDigest,
	}, deploymentID)
	if err != nil {
		return "", err
	}
	_ = s.runs.SetState(ctx, runID, runs.Running, "", "")
	go s.applyConfiguration(context.WithoutCancel(ctx), runID, deploymentID, row.DesiredState == "running", plan.Deployments[0])
	return runID, nil
}

func (s *Service) applyConfiguration(ctx context.Context, runID, deploymentID string, wasRunning bool, reviewed RepositoryUpdateDeployment) {
	fail := func(code string, err error) {
		_ = s.runs.Complete(ctx, runID, runs.Failed, code, err.Error())
	}
	if wasRunning {
		if _, err := s.Stop(ctx, deploymentID); err != nil {
			fail("deployment.stop_failed", err)
			return
		}
		for {
			row, err := s.q.GetDeployment(ctx, deploymentID)
			if err != nil {
				fail("deployment.stop_failed", err)
				return
			}
			if row.ObservedState == "stopped" {
				break
			}
			if err := waitUpdatePoll(ctx); err != nil {
				fail("deployment.stop_failed", err)
				return
			}
		}
	}
	// Plan again now the deployment's own leases are free; the reviewed
	// placement, workload and fabric must still hold.
	reviewedPlan := reviewed.DeploymentPlan
	fresh, err := s.plan(ctx, PlanRequest{
		RecipeDigest: reviewedPlan.RecipeDigest, Parameters: reviewedPlan.Parameters,
		Placements: planOverrides(reviewedPlan.Placements), Variants: reviewedPlan.Variants,
		WorkloadIndex: &reviewedPlan.WorkloadIndex, AcquisitionPolicy: valueOrPolicy(reviewedPlan.AcquisitionPolicy),
	}, nil, false)
	if err == nil && (!fresh.Ready || preservedPlacementMismatch(reviewedPlan.Placements, fresh.Placements) != "" ||
		fresh.WorkloadIndex != reviewedPlan.WorkloadIndex || valueOr(fresh.Fabric, "") != reviewed.Fabric) {
		err = fmt.Errorf("%w: the reviewed placement, workload or fabric no longer holds", ErrNotReady)
	}
	if err != nil {
		fail("deployment.configuration_failed", err)
		return
	}
	var fabric sql.NullString
	if fresh.Fabric != nil {
		fabric = sql.NullString{String: *fresh.Fabric, Valid: true}
	}
	placement := placementSetFromPlan(fresh)
	updated, err := s.q.UpdateStoppedDeploymentConfiguration(ctx, db.UpdateStoppedDeploymentConfigurationParams{
		Parameters: marshalParameters(fresh.Parameters), Placement: placement.Marshal(), Fabric: fabric, ID: deploymentID,
	})
	if err == nil && updated == 0 {
		err = fmt.Errorf("%w: deployment was started or removed before its settings were saved", ErrState)
	}
	if err != nil {
		fail("deployment.configuration_failed", err)
		return
	}
	s.bus.Publish(ctx, "deployment.state", deploymentID, mustJSON(map[string]any{
		"deployment_id": deploymentID, "reason": "configuration",
	}))
	if wasRunning {
		if _, err := s.Start(ctx, deploymentID); err != nil {
			fail("deployment.start_failed", err)
			return
		}
		// Done means serving with the new settings, not merely dispatched.
		for {
			row, err := s.q.GetDeployment(ctx, deploymentID)
			if err != nil {
				fail("deployment.start_failed", err)
				return
			}
			if row.ObservedState == "healthy" {
				break
			}
			if row.ObservedState == "failed" || row.DesiredState != "running" {
				fail("deployment.start_failed", fmt.Errorf("deployment %s is %s with the new settings", deploymentID, row.ObservedState))
				return
			}
			if err := waitUpdatePoll(ctx); err != nil {
				fail("deployment.start_failed", err)
				return
			}
		}
	}
	_ = s.runs.SetState(ctx, runID, runs.Verifying, "", "")
	_ = s.runs.Complete(ctx, runID, runs.Succeeded, "", "")
}
