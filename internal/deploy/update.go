package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/runs"
)

const repositoryUpdatePollInterval = 250 * time.Millisecond

var errRepositoryUpdateCancelled = errors.New("repository update cancelled")

// RepositoryUpdateTarget is one persisted hardware row in an update preview.
type RepositoryUpdateTarget struct {
	SourceDeploymentID      string `json:"source_deployment_id"`
	ReplacementDeploymentID string `json:"replacement_deployment_id,omitempty"`
	NodeID                  string `json:"node_id"`
	NodeName                string `json:"node_name"`
	NodeStatus              string `json:"node_status"`
	Rank                    int32  `json:"rank"`
	Status                  string `json:"status,omitempty"`
	Phase                   string `json:"phase,omitempty"`
	CurrentStep             int    `json:"current_step,omitempty"`
	TotalSteps              int    `json:"total_steps,omitempty"`
	ErrorCode               string `json:"error_code,omitempty"`
	ErrorMessage            string `json:"error_message,omitempty"`
}

// RepositoryUpdateDeployment retains the complete source deployment contract
// and the exact replacement plan used after the source is stopped.
type RepositoryUpdateDeployment struct {
	SourceDeploymentID string            `json:"source_deployment_id"`
	SourceDigest       string            `json:"source_digest"`
	Profile            string            `json:"profile"`
	Placement          string            `json:"placement"`
	Fabric             string            `json:"fabric,omitempty"`
	WorkloadIndex      int               `json:"workload_index"`
	Variants           map[string]string `json:"variants,omitempty"`
	DeploymentPlan     Plan              `json:"deployment_plan"`
}

// RepositoryUpdatePlan previews every desired-running deployment that still
// uses an older immutable package from one logical repository.
type RepositoryUpdatePlan struct {
	RepositoryID string                       `json:"repository_id"`
	TargetDigest string                       `json:"target_digest"`
	Deployments  []RepositoryUpdateDeployment `json:"deployments"`
	Targets      []RepositoryUpdateTarget     `json:"targets"`
	Diagnostics  []diag.Diagnostic            `json:"diagnostics,omitempty"`
	Ready        bool                         `json:"ready"`
	Digest       string                       `json:"plan_digest"`
}

func (p *RepositoryUpdatePlan) PlanDigest() string {
	copy := *p
	copy.Digest = ""
	encoded, _ := json.Marshal(copy)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PlanRepositoryUpdate preserves placements, profile, variants, workload,
// fabric, endpoint intent, and ignores only the source deployment's leases.
func (s *Service) PlanRepositoryUpdate(ctx context.Context, repositoryID, targetDigest string) (*RepositoryUpdatePlan, error) {
	targetVersion, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, targetDigest)
	if err != nil || targetVersion.RepositoryID != repositoryID {
		return nil, fmt.Errorf("%w: target digest is not a version of repository", ErrRecipe)
	}
	rows, err := s.q.ListRepositoryActiveDeployments(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	plan := &RepositoryUpdatePlan{RepositoryID: repositoryID, TargetDigest: targetDigest, Ready: true}
	for _, row := range rows {
		if row.RecipeDigest == targetDigest {
			continue
		}
		placementSet := ParsePlacementSet(row.Placement)
		placements := append([]Placement(nil), placementSet.Entries...)
		if len(placements) == 0 {
			for nodeID, rank := range placementSet.Ranks {
				node, nodeErr := s.q.GetNode(ctx, nodeID)
				if nodeErr != nil {
					return nil, nodeErr
				}
				placements = append(placements, Placement{NodeID: nodeID, NodeName: node.DisplayName, Rank: int32(rank)})
			}
			sort.Slice(placements, func(i, j int) bool { return placements[i].Rank < placements[j].Rank })
		}
		overrides := make([]PlacementOverride, 0, len(placements))
		for _, placement := range placements {
			overrides = append(overrides, PlacementOverride{NodeID: placement.NodeID, Rank: placement.Rank})
		}
		deploymentPlan, planErr := s.plan(ctx, PlanRequest{
			RecipeDigest: targetDigest, Profile: row.Profile,
			Placements: overrides, Variants: placementSet.Variants,
		}, map[string]bool{row.ID: true}, true)
		if planErr != nil {
			return nil, planErr
		}
		if mismatch := preservedPlacementMismatch(placements, deploymentPlan.Placements); mismatch != "" {
			deploymentPlan.Ready = false
			deploymentPlan.Diagnostics = append(deploymentPlan.Diagnostics, diag.Diagnostic{
				Code: "recipe.update_placement_changed", Severity: "error", Message: mismatch,
			})
		}
		workloadIndex := 0
		if placementSet.Workload != nil {
			workloadIndex = *placementSet.Workload
			if deploymentPlan.WorkloadIndex != workloadIndex {
				deploymentPlan.Ready = false
				deploymentPlan.Diagnostics = append(deploymentPlan.Diagnostics, diag.Diagnostic{
					Code: "recipe.update_workload_changed", Severity: "error", Message: "target recipe cannot preserve the selected workload",
				})
			}
		}
		fabric := ""
		if row.Fabric.Valid {
			fabric = row.Fabric.String
		}
		plannedFabric := ""
		if deploymentPlan.Fabric != nil {
			plannedFabric = *deploymentPlan.Fabric
		}
		if fabric != plannedFabric {
			deploymentPlan.Ready = false
			deploymentPlan.Diagnostics = append(deploymentPlan.Diagnostics, diag.Diagnostic{
				Code: "recipe.update_fabric_changed", Severity: "error", Message: "target recipe cannot preserve the selected fabric",
			})
		}
		deploymentPlan.Digest = deploymentPlan.PlanDigest()
		plan.Deployments = append(plan.Deployments, RepositoryUpdateDeployment{
			SourceDeploymentID: row.ID, SourceDigest: row.RecipeDigest, Profile: row.Profile,
			Placement: row.Placement, Fabric: fabric, WorkloadIndex: workloadIndex,
			Variants: cloneVariants(placementSet.Variants), DeploymentPlan: *deploymentPlan,
		})
		for _, placement := range placements {
			node, nodeErr := s.q.GetNode(ctx, placement.NodeID)
			if nodeErr != nil {
				return nil, nodeErr
			}
			plan.Targets = append(plan.Targets, RepositoryUpdateTarget{
				SourceDeploymentID: row.ID, NodeID: placement.NodeID,
				NodeName: node.DisplayName, NodeStatus: node.Status, Rank: placement.Rank,
				Status: "pending", Phase: "fetching", TotalSteps: 5,
			})
		}
		if !deploymentPlan.Ready {
			plan.Ready = false
			plan.Diagnostics = append(plan.Diagnostics, deploymentPlan.Diagnostics...)
		}
	}
	sort.Slice(plan.Targets, func(i, j int) bool {
		if plan.Targets[i].SourceDeploymentID == plan.Targets[j].SourceDeploymentID {
			return plan.Targets[i].Rank < plan.Targets[j].Rank
		}
		return plan.Targets[i].SourceDeploymentID < plan.Targets[j].SourceDeploymentID
	})
	plan.Digest = plan.PlanDigest()
	return plan, nil
}

func preservedPlacementMismatch(source, target []Placement) string {
	if len(source) != len(target) {
		return "target recipe changes the hardware rank count"
	}
	byRank := make(map[int32]Placement, len(target))
	for _, placement := range target {
		byRank[placement.Rank] = placement
	}
	for _, expected := range source {
		actual, ok := byRank[expected.Rank]
		if !ok || actual.NodeID != expected.NodeID {
			return fmt.Sprintf("rank %d cannot remain on node %s", expected.Rank, expected.NodeID)
		}
		if expected.AcceleratorUUID != "" && actual.AcceleratorUUID != expected.AcceleratorUUID {
			return fmt.Sprintf("rank %d cannot retain accelerator %s", expected.Rank, expected.AcceleratorUUID)
		}
	}
	return ""
}

func cloneVariants(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

type repositoryUpdateRunInput struct {
	RepositoryID string                   `json:"repository_id"`
	FromDigest   string                   `json:"from_digest"`
	FromCommit   string                   `json:"from_commit"`
	ToCommit     string                   `json:"to_commit"`
	TargetDigest string                   `json:"target_digest"`
	Targets      []RepositoryUpdateTarget `json:"targets"`
	Plan         RepositoryUpdatePlan     `json:"plan"`
}

type repositoryUpdateProgress struct {
	Phase             string                   `json:"phase"`
	TotalHardware     int                      `json:"total_hardware"`
	CompletedHardware int                      `json:"completed_hardware"`
	Hardware          []RepositoryUpdateTarget `json:"hardware"`
}

// CreateRepositoryUpdate validates a fresh plan before any source deployment
// is stopped, persists the run, and starts the restart-safe coordinator.
func (s *Service) CreateRepositoryUpdate(ctx context.Context, repositoryID, targetDigest, planDigest string) (string, error) {
	plan, err := s.PlanRepositoryUpdate(ctx, repositoryID, targetDigest)
	if err != nil {
		return "", err
	}
	if plan.Digest != planDigest {
		return "", fmt.Errorf("%w: %s != %s", ErrPlanStale, planDigest, plan.Digest)
	}
	if !plan.Ready {
		return "", fmt.Errorf("%w: repository update plan is not ready", ErrNotReady)
	}
	input := repositoryUpdateRunInput{RepositoryID: repositoryID, TargetDigest: targetDigest, Targets: plan.Targets, Plan: *plan}
	if len(plan.Deployments) > 0 {
		input.FromDigest = plan.Deployments[0].SourceDigest
		if version, versionErr := s.q.GetRecipeRepositoryVersionByDigest(ctx, input.FromDigest); versionErr == nil {
			input.FromCommit = version.CommitSha
		}
	}
	if version, versionErr := s.q.GetRecipeRepositoryVersionByDigest(ctx, targetDigest); versionErr == nil {
		input.ToCommit = version.CommitSha
	}
	runID, err := s.runs.Create(ctx, "library", "recipe-update", structMap(input), "")
	if err != nil {
		return "", err
	}
	progress := repositoryUpdateProgress{
		Phase: "installing_recipe", TotalHardware: len(plan.Targets),
		Hardware: append([]RepositoryUpdateTarget(nil), plan.Targets...),
	}
	if err := s.runs.SetProgress(ctx, runID, structMap(progress)); err != nil {
		return "", err
	}
	_ = s.runs.SetState(ctx, runID, runs.Planning, "", "")
	_ = s.runs.SetState(ctx, runID, runs.Waiting, "", "")
	s.startRepositoryUpdate(ctx, runID)
	return runID, nil
}

// CancelRepositoryUpdate requests rollback; the run remains nonterminal until
// every stopped source deployment is restored or restoration fails.
func (s *Service) CancelRepositoryUpdate(ctx context.Context, runID string) error {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Kind != "recipe-update" {
		return fmt.Errorf("%w: run is %s", ErrState, run.Kind)
	}
	return s.runs.Cancel(ctx, runID)
}

// RunRepositoryUpdateCoordinator resumes all nonterminal update runs and then
// relies on each run's persisted input/progress for convergence.
func (s *Service) RunRepositoryUpdateCoordinator(ctx context.Context) {
	rows, err := s.q.ListRecipeUpdateRuns(ctx)
	if err != nil {
		return
	}
	for _, row := range rows {
		s.startRepositoryUpdate(ctx, row.ID)
	}
}

func (s *Service) startRepositoryUpdate(ctx context.Context, runID string) {
	s.updateMu.Lock()
	if s.updateLive[runID] {
		s.updateMu.Unlock()
		return
	}
	s.updateLive[runID] = true
	s.updateMu.Unlock()
	go func() {
		defer func() {
			s.updateMu.Lock()
			delete(s.updateLive, runID)
			s.updateMu.Unlock()
		}()
		s.coordinateRepositoryUpdate(ctx, runID)
	}()
}

func (s *Service) coordinateRepositoryUpdate(ctx context.Context, runID string) {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return
	}
	var input repositoryUpdateRunInput
	if err := mapStruct(run.Input, &input); err != nil {
		_ = s.runs.Complete(ctx, runID, runs.Failed, "recipe.update_input_invalid", err.Error())
		return
	}
	var progress repositoryUpdateProgress
	if err := mapStruct(run.Progress, &progress); err != nil || len(progress.Hardware) == 0 && len(input.Targets) > 0 {
		progress = repositoryUpdateProgress{Phase: "installing_recipe", TotalHardware: len(input.Targets), Hardware: append([]RepositoryUpdateTarget(nil), input.Targets...)}
	}
	if run.State != string(runs.Cancelling) {
		_ = s.runs.SetState(ctx, runID, runs.Running, "", "")
	}

	for _, deployment := range input.Plan.Deployments {
		if s.updateCancelled(ctx, runID) {
			err = errRepositoryUpdateCancelled
			break
		}
		if err = s.stopUpdateSource(ctx, runID, deployment, &progress); err != nil {
			break
		}
		if s.updateCancelled(ctx, runID) {
			err = errRepositoryUpdateCancelled
			break
		}
		var replacementID string
		replacementID, err = s.ensureUpdateReplacement(ctx, runID, deployment, input.TargetDigest, &progress)
		if err != nil {
			break
		}
		if err = s.waitUpdateReplacement(ctx, runID, deployment, replacementID, &progress); err != nil {
			break
		}
	}
	if err == nil {
		progress.Phase = "ready"
		progress.CompletedHardware = progress.TotalHardware
		for i := range progress.Hardware {
			progress.Hardware[i].Status = "succeeded"
			progress.Hardware[i].Phase = "ready"
			progress.Hardware[i].CurrentStep = 5
		}
		s.persistUpdateProgress(ctx, runID, &progress)
		_ = s.runs.SetOutput(ctx, runID, map[string]any{"targets": updateOutputTargets(progress.Hardware)})
		_ = s.runs.SetState(ctx, runID, runs.Verifying, "", "")
		_ = s.runs.Complete(ctx, runID, runs.Succeeded, "", "")
		return
	}

	rollbackErr := s.rollbackRepositoryUpdate(ctx, runID, input.Plan.Deployments, &progress)
	if rollbackErr != nil {
		_ = s.runs.SetOutput(ctx, runID, map[string]any{"targets": updateOutputTargets(progress.Hardware)})
		_ = s.runs.Complete(ctx, runID, runs.Failed, "recipe.rollback_failed", rollbackErr.Error())
		return
	}
	_ = s.runs.SetOutput(ctx, runID, map[string]any{"targets": updateOutputTargets(progress.Hardware)})
	if errors.Is(err, errRepositoryUpdateCancelled) || s.updateCancelled(ctx, runID) {
		_ = s.runs.Complete(ctx, runID, runs.Cancelled, "run.cancelled", "recipe update cancelled and source deployments restored")
		return
	}
	_ = s.runs.Complete(ctx, runID, runs.Failed, "recipe.update_failed", err.Error())
}

func (s *Service) stopUpdateSource(ctx context.Context, runID string, deployment RepositoryUpdateDeployment, progress *repositoryUpdateProgress) error {
	s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
		target.Status, target.Phase = "running", "stopping_old"
	})
	progress.Phase = "stopping_old"
	s.persistUpdateProgress(ctx, runID, progress)
	row, err := s.q.GetDeployment(ctx, deployment.SourceDeploymentID)
	if err != nil {
		return err
	}
	if row.DesiredState == "running" {
		if _, err := s.Stop(ctx, deployment.SourceDeploymentID); err != nil && !errors.Is(err, ErrState) {
			return err
		}
	}
	for {
		row, err = s.q.GetDeployment(ctx, deployment.SourceDeploymentID)
		if err != nil {
			return err
		}
		if row.ObservedState == "stopped" {
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
				target.CurrentStep = maxInt(target.CurrentStep, 1)
				target.Phase = "preparing"
			})
			s.persistUpdateProgress(ctx, runID, progress)
			return nil
		}
		if s.anyUpdateNodeOffline(progress, deployment.SourceDeploymentID) {
			progress.Phase = "waiting_offline"
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) { target.Status, target.Phase = "waiting", "waiting_offline" })
			s.persistUpdateProgress(ctx, runID, progress)
		}
		if err := waitUpdatePoll(ctx); err != nil {
			return err
		}
	}
}

func (s *Service) ensureUpdateReplacement(ctx context.Context, runID string, deployment RepositoryUpdateDeployment, targetDigest string, progress *repositoryUpdateProgress) (string, error) {
	for _, target := range progress.Hardware {
		if target.SourceDeploymentID == deployment.SourceDeploymentID && target.ReplacementDeploymentID != "" {
			return target.ReplacementDeploymentID, nil
		}
	}
	expectedPlacement := placementSet{
		Ranks: map[string]int{}, Entries: deployment.DeploymentPlan.Placements,
		Workload: &deployment.WorkloadIndex, Variants: deployment.Variants,
	}
	for _, entry := range expectedPlacement.Entries {
		expectedPlacement.Ranks[entry.NodeID] = int(entry.Rank)
	}
	existing, existingErr := s.q.GetDeploymentByRecipeProfilePlacement(ctx, db.GetDeploymentByRecipeProfilePlacementParams{
		RecipeDigest: targetDigest, Profile: deployment.Profile, Placement: expectedPlacement.Marshal(),
	})
	var replacementID string
	if existingErr == nil {
		replacementID = existing.ID
		if existing.DesiredState == "stopped" && existing.ObservedState == "stopped" {
			if _, err := s.Start(ctx, existing.ID); err != nil {
				return "", err
			}
		}
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return "", existingErr
	} else {
		fresh, err := s.plan(ctx, PlanRequest{
			RecipeDigest: targetDigest, Profile: deployment.Profile,
			Placements: planOverrides(deployment.DeploymentPlan.Placements), Variants: deployment.Variants,
		}, nil, true)
		if err != nil {
			return "", err
		}
		if fresh.Digest != deployment.DeploymentPlan.Digest {
			return "", fmt.Errorf("%w: replacement plan changed before create", ErrPlanStale)
		}
		replacement, err := s.createPlanned(ctx, fresh)
		if err != nil {
			return "", err
		}
		replacementID = replacement.ID
	}
	s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
		target.ReplacementDeploymentID = replacementID
		target.Status, target.Phase = "running", "preparing"
	})
	progress.Phase = "preparing"
	s.persistUpdateProgress(ctx, runID, progress)
	return replacementID, nil
}

func (s *Service) waitUpdateReplacement(ctx context.Context, runID string, deployment RepositoryUpdateDeployment, replacementID string, progress *repositoryUpdateProgress) error {
	for {
		if s.updateCancelled(ctx, runID) {
			return errRepositoryUpdateCancelled
		}
		row, err := s.q.GetDeployment(ctx, replacementID)
		if err != nil {
			return err
		}
		dispatch := ParseDispatch(row.Dispatch)
		s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
			step, phase := updateStepForDispatch(dispatch.Get(target.Rank))
			if step > target.CurrentStep {
				target.CurrentStep = step
			}
			target.Status, target.Phase = "running", phase
			if row.ObservedState == "healthy" {
				target.CurrentStep, target.Status, target.Phase = 5, "succeeded", "ready"
			}
		})
		progress.CompletedHardware = countSucceededHardware(progress.Hardware)
		progress.Phase = updateOverallPhase(progress.Hardware, deployment.SourceDeploymentID)
		s.persistUpdateProgress(ctx, runID, progress)
		if row.ObservedState == "healthy" {
			return nil
		}
		if row.ObservedState == "failed" || row.DesiredState != "running" {
			return fmt.Errorf("replacement deployment %s is %s", replacementID, row.ObservedState)
		}
		if row.RunID.Valid {
			replacementRun, runErr := s.runs.Get(ctx, row.RunID.String)
			if runErr == nil && (replacementRun.State == string(runs.Failed) || replacementRun.State == string(runs.Interrupted)) {
				return fmt.Errorf("replacement deployment failed: %s", valueOr(replacementRun.ErrorMessage, replacementRun.State))
			}
		}
		if s.anyUpdateNodeOffline(progress, deployment.SourceDeploymentID) {
			progress.Phase = "waiting_offline"
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) { target.Status, target.Phase = "waiting", "waiting_offline" })
			s.persistUpdateProgress(ctx, runID, progress)
		}
		if err := waitUpdatePoll(ctx); err != nil {
			return err
		}
	}
}

func (s *Service) rollbackRepositoryUpdate(ctx context.Context, runID string, deployments []RepositoryUpdateDeployment, progress *repositoryUpdateProgress) error {
	progress.Phase = "rolling_back"
	s.persistUpdateProgress(ctx, runID, progress)
	var rollbackErrors []string
	for index := len(deployments) - 1; index >= 0; index-- {
		deployment := deployments[index]
		replacementID := ""
		for _, target := range progress.Hardware {
			if target.SourceDeploymentID == deployment.SourceDeploymentID && target.ReplacementDeploymentID != "" {
				replacementID = target.ReplacementDeploymentID
				break
			}
		}
		if replacementID != "" {
			replacement, err := s.q.GetDeployment(ctx, replacementID)
			if err == nil && replacement.DesiredState == "running" {
				_, _ = s.Stop(ctx, replacementID)
			}
			for err == nil && replacement.ObservedState != "stopped" {
				progress.Phase = "rolling_back"
				s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) { target.Status, target.Phase = "running", "rolling_back" })
				s.persistUpdateProgress(ctx, runID, progress)
				if waitErr := waitUpdatePoll(ctx); waitErr != nil {
					return waitErr
				}
				replacement, err = s.q.GetDeployment(ctx, replacementID)
			}
			if err != nil {
				rollbackErrors = append(rollbackErrors, err.Error())
			}
		}

		progress.Phase = "restoring_old"
		s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) { target.Status, target.Phase = "running", "restoring_old" })
		s.persistUpdateProgress(ctx, runID, progress)
		source, err := s.q.GetDeployment(ctx, deployment.SourceDeploymentID)
		if err != nil {
			rollbackErrors = append(rollbackErrors, err.Error())
			continue
		}
		for source.ObservedState != "stopped" && source.DesiredState == "stopped" {
			if waitErr := waitUpdatePoll(ctx); waitErr != nil {
				return waitErr
			}
			source, err = s.q.GetDeployment(ctx, deployment.SourceDeploymentID)
			if err != nil {
				break
			}
		}
		if err == nil && source.DesiredState == "stopped" {
			_, err = s.Start(ctx, deployment.SourceDeploymentID)
		}
		if err != nil {
			rollbackErrors = append(rollbackErrors, err.Error())
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
				target.Status, target.Phase = "failed", "rollback_failed"
				target.ErrorCode, target.ErrorMessage = "recipe.rollback_failed", err.Error()
			})
			continue
		}
		for {
			source, err = s.q.GetDeployment(ctx, deployment.SourceDeploymentID)
			if err != nil || source.ObservedState == "healthy" {
				break
			}
			if source.ObservedState == "failed" {
				err = fmt.Errorf("source deployment %s failed during restore", source.ID)
				break
			}
			if waitErr := waitUpdatePoll(ctx); waitErr != nil {
				return waitErr
			}
		}
		if err != nil {
			rollbackErrors = append(rollbackErrors, err.Error())
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) {
				target.Status, target.Phase = "failed", "rollback_failed"
				target.ErrorCode, target.ErrorMessage = "recipe.rollback_failed", err.Error()
			})
		} else {
			s.setUpdateTargets(progress, deployment.SourceDeploymentID, func(target *RepositoryUpdateTarget) { target.Status, target.Phase = "failed", "restored" })
		}
		s.persistUpdateProgress(ctx, runID, progress)
	}
	if len(rollbackErrors) > 0 {
		progress.Phase = "rollback_failed"
		s.persistUpdateProgress(ctx, runID, progress)
		return errors.New(strings.Join(rollbackErrors, "; "))
	}
	progress.Phase = "restored"
	s.persistUpdateProgress(ctx, runID, progress)
	return nil
}

func (s *Service) setUpdateTargets(progress *repositoryUpdateProgress, sourceDeploymentID string, update func(*RepositoryUpdateTarget)) {
	for index := range progress.Hardware {
		if progress.Hardware[index].SourceDeploymentID == sourceDeploymentID {
			update(&progress.Hardware[index])
		}
	}
}

func (s *Service) persistUpdateProgress(ctx context.Context, runID string, progress *repositoryUpdateProgress) {
	progress.CompletedHardware = countSucceededHardware(progress.Hardware)
	_ = s.runs.SetProgress(ctx, runID, structMap(*progress))
}

func (s *Service) anyUpdateNodeOffline(progress *repositoryUpdateProgress, sourceDeploymentID string) bool {
	for index := range progress.Hardware {
		target := &progress.Hardware[index]
		if target.SourceDeploymentID != sourceDeploymentID {
			continue
		}
		node, err := s.q.GetNode(context.Background(), target.NodeID)
		if err == nil {
			target.NodeStatus = node.Status
		}
		if !s.nodes.Online(target.NodeID) {
			return true
		}
	}
	return false
}

func (s *Service) updateCancelled(ctx context.Context, runID string) bool {
	run, err := s.runs.Get(ctx, runID)
	return err == nil && run.State == string(runs.Cancelling)
}

func updateStepForDispatch(phase string) (int, string) {
	switch phase {
	case PhasePrepared:
		return 2, "pulling"
	case PhasePulled:
		return 3, "starting"
	case PhaseCreated, PhaseVerifying:
		return 3, "starting"
	case PhaseStarted:
		return 4, "verifying"
	default:
		return 1, "preparing"
	}
}

func updateOverallPhase(targets []RepositoryUpdateTarget, sourceDeploymentID string) string {
	phase := "preparing"
	for _, target := range targets {
		if target.SourceDeploymentID == sourceDeploymentID && target.Phase != "" {
			phase = target.Phase
		}
	}
	return phase
}

func countSucceededHardware(targets []RepositoryUpdateTarget) int {
	count := 0
	for _, target := range targets {
		if target.Status == "succeeded" {
			count++
		}
	}
	return count
}

func updateOutputTargets(targets []RepositoryUpdateTarget) []map[string]any {
	output := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		status := "updated"
		if target.Status != "succeeded" {
			status = target.Phase
		}
		output = append(output, map[string]any{
			"source_deployment_id":      target.SourceDeploymentID,
			"replacement_deployment_id": target.ReplacementDeploymentID,
			"node_id":                   target.NodeID, "rank": target.Rank, "status": status,
		})
	}
	return output
}

func planOverrides(placements []Placement) []PlacementOverride {
	overrides := make([]PlacementOverride, 0, len(placements))
	for _, placement := range placements {
		overrides = append(overrides, PlacementOverride{NodeID: placement.NodeID, Rank: placement.Rank})
	}
	return overrides
}

func structMap(value any) map[string]any {
	encoded, _ := json.Marshal(value)
	result := map[string]any{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func mapStruct(value map[string]any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func waitUpdatePoll(ctx context.Context) error {
	timer := time.NewTimer(repositoryUpdatePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func valueOr(value *string, fallback string) string {
	if value != nil && *value != "" {
		return *value
	}
	return fallback
}
