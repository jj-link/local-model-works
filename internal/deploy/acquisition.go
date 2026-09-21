package deploy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/runtime"
)

type acquisitionService interface {
	Plan(context.Context, downloads.PlanRequest) (*downloads.Plan, error)
	Acquire(context.Context, downloads.PlanRequest, bool) error
	Lock(context.Context, string, string, string, string, downloads.OwnerKind, string, string, string) error
	MarkCancelling(context.Context, string, string, downloads.OwnerKind, string) error
	Unlock(context.Context, string, string, downloads.OwnerKind, string, downloads.QuiescenceProof) error
	SupportsNode(context.Context, string) bool
	PreparePeer(context.Context, downloads.PeerRequest) (*downloads.PreparedPeer, error)
}

// SetDownloads supplies the shared, inspection-first acquisition owner.
func (s *Service) SetDownloads(service *downloads.Service) {
	if service == nil {
		s.downloads = nil
		return
	}
	s.downloads = service
}

func acquisitionPolicy(policy string) (string, error) {
	switch policy {
	case "", AcquisitionRequireExisting:
		return AcquisitionRequireExisting, nil
	case AcquisitionDownloadMissing:
		return policy, nil
	default:
		return "", fmt.Errorf("%w: unknown acquisition_policy %q", ErrNoTarget, policy)
	}
}

func acquisitionRequest(digest string, ps placementSet) (downloads.PlanRequest, error) {
	if ps.Workload == nil {
		return downloads.PlanRequest{}, fmt.Errorf("%w: deployment has no persisted workload selection", ErrNotReady)
	}
	byNode := make(map[string]downloads.Target, len(ps.Entries))
	for _, placement := range ps.Entries {
		if placement.NodeID == "" {
			return downloads.PlanRequest{}, fmt.Errorf("%w: acquisition requires explicit target nodes", ErrNotReady)
		}
		byNode[placement.NodeID] = downloads.Target{NodeID: placement.NodeID}
	}
	for _, target := range ps.AcquisitionTargets {
		if _, ok := byNode[target.NodeID]; !ok {
			return downloads.PlanRequest{}, fmt.Errorf("%w: acquisition target differs from persisted ranks", ErrNotReady)
		}
		byNode[target.NodeID] = target
	}
	if len(byNode) == 0 {
		return downloads.PlanRequest{}, fmt.Errorf("%w: acquisition requires explicit target nodes", ErrNotReady)
	}
	targets := make([]downloads.Target, 0, len(byNode))
	for _, target := range byNode {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].NodeID < targets[j].NodeID })
	request := downloads.PlanRequest{RecipeDigest: digest, Targets: targets, WorkloadIndex: ps.Workload, Variants: ps.Variants}
	if len(ps.AcquisitionResources) != 0 {
		request.ReviewedPlan = &downloads.Plan{RecipeDigest: digest, WorkloadIndex: *ps.Workload, Variants: ps.Variants, Targets: targets, Resources: ps.AcquisitionResources}
	}
	return request, nil
}

func (s *Service) planAcquisition(ctx context.Context, plan *Plan) error {
	if s.downloads == nil {
		return fmt.Errorf("%w: shared downloads service is unavailable", ErrNotReady)
	}
	req, err := acquisitionRequest(plan.RecipeDigest, placementSetFromPlan(plan))
	if err != nil {
		return err
	}
	acquisition, err := s.downloads.Plan(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: acquisition preview: %v", ErrNotReady, err)
	}
	if acquisition == nil {
		return fmt.Errorf("%w: acquisition preview is unavailable", ErrNotReady)
	}
	plan.Acquisition = acquisition
	plan.Diagnostics = append(plan.Diagnostics, acquisition.Diagnostics...)
	for _, resource := range acquisition.Resources {
		if resource.Required && (resource.Verification.State != downloads.ResourceAvailable || resource.Verification.Stale || resource.Verification.VerifiedAt == nil) {
			plan.MissingResources = append(plan.MissingResources, resource)
		}
	}
	if plan.AcquisitionPolicy == AcquisitionRequireExisting && len(plan.MissingResources) != 0 {
		plan.Diagnostics = append(plan.Diagnostics, diag.Error("acquisition.missing_resources", "required resources are not exactly available on every selected node; review download-missing to acquire them"))
	}
	if !acquisition.Ready && !diag.HasError(acquisition.Diagnostics) {
		plan.Diagnostics = append(plan.Diagnostics, diag.Error("acquisition.not_ready", "required acquisition resources cannot be resolved or prepared"))
	}
	return nil
}

// Precommit checks must never download: download-missing starts acquisition only
// after the reviewed deployment and its resource leases have been committed.
func (s *Service) acquirePlan(ctx context.Context, plan *Plan, precommit bool) error {
	policy, err := acquisitionPolicy(plan.AcquisitionPolicy)
	if err != nil {
		return err
	}
	if precommit && policy == AcquisitionDownloadMissing {
		return nil
	}
	if s.downloads == nil {
		return fmt.Errorf("%w: shared downloads service is unavailable", ErrNotReady)
	}
	req, err := acquisitionRequest(plan.RecipeDigest, placementSetFromPlan(plan))
	if err != nil {
		return err
	}
	if err := s.downloads.Acquire(ctx, req, policy == AcquisitionRequireExisting); err != nil {
		return fmt.Errorf("%w: acquisition: %v", ErrNotReady, err)
	}
	return nil
}

func (s *Service) acquireDeployment(ctx context.Context, row db.GetDeploymentRow) error {
	s.mu.Lock()
	ownership := s.acquisitionLocks[row.ID]
	if ownership == nil {
		ownership = &sync.Mutex{}
		s.acquisitionLocks[row.ID] = ownership
	}
	s.mu.Unlock()
	ownership.Lock()
	defer ownership.Unlock()
	current, err := s.q.GetDeployment(ctx, row.ID)
	if err != nil {
		return err
	}
	if current.DesiredState != "running" || current.RunID.String != row.RunID.String {
		return context.Canceled
	}
	row = current
	if s.downloads == nil {
		return fmt.Errorf("shared downloads service is unavailable")
	}
	ps := ParsePlacementSet(row.Placement)
	policy, err := acquisitionPolicy(ps.AcquisitionPolicy)
	if err != nil {
		return err
	}
	req, err := acquisitionRequest(row.RecipeDigest, ps)
	if err != nil {
		return err
	}
	return s.downloads.Acquire(ctx, req, policy == AcquisitionRequireExisting)
}

// Acquiring from an agent result handler must not block that agent's reader:
// INSPECT and acquisition completion arrive on the same stream. Coalesce rank
// wakeups while one gate runs; every subsequent phase repeats exact inspection.
func (s *Service) dispatchNext(ctx context.Context, depID string, rank int32, runID string, pl Placement) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	if row.DesiredState != "running" {
		s.dispatchReady(ctx, depID, rank, runID, pl)
		return
	}
	key := fmt.Sprintf("%s|%d", depID, rank)
	s.mu.Lock()
	if _, active := s.acquisitionLive[key]; active {
		s.acquisitionLive[key] = true
		s.mu.Unlock()
		return
	}
	s.acquisitionLive[key] = false
	s.mu.Unlock()
	go func() {
		gateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
		defer cancel()
		for {
			current, err := s.q.GetDeployment(gateCtx, depID)
			if err == nil && current.DesiredState == "running" && current.RunID.String == runID {
				// Reconciliation of an already-started container is read-only.
				// Losing an acquisition source must not stop an existing workload.
				switch ParseDispatch(current.Dispatch).Get(rank) {
				case PhaseStarted, PhaseStopped, PhasePrepared:
				default:
					err = s.acquireDeployment(gateCtx, current)
				}
				// A stop or replacement during acquisition cannot authorize a launch.
				latest, readErr := s.q.GetDeployment(gateCtx, depID)
				if readErr == nil && latest.DesiredState == "running" && latest.RunID.String == runID {
					if err != nil {
						s.failDispatch(gateCtx, depID, rank, runID, "acquisition.unavailable", err.Error())
						_, _ = s.Stop(gateCtx, depID)
					} else {
						s.dispatchReady(gateCtx, depID, rank, runID, pl)
					}
				}
			}
			s.mu.Lock()
			pending := s.acquisitionLive[key]
			if pending && err == nil {
				s.acquisitionLive[key] = false
				s.mu.Unlock()
				continue
			}
			delete(s.acquisitionLive, key)
			s.mu.Unlock()
			return
		}
	}()
}

func acquisitionPath(ps placementSet, nodeID, identity string) (string, error) {
	for _, resource := range ps.AcquisitionResources {
		if resource.NodeID == nodeID && resource.Identity == identity && resource.Kind != downloads.ResourceImage && resource.Destination != "" {
			return resource.Destination, nil
		}
	}
	return "", fmt.Errorf("resource %s has no reviewed destination on node %s", identity, nodeID)
}

func applyAcquisitionSpec(spec *runtime.ContainerSpec, ps placementSet, nodeID string) error {
	imageFound := false
	packageFound := false
	for _, resource := range ps.AcquisitionResources {
		if resource.NodeID != nodeID || !resource.Required {
			continue
		}
		spec.AcquisitionResources = append(spec.AcquisitionResources, resource.ResourceSpec)
		if resource.Kind == downloads.ResourceRecipe && resource.Identity == "recipe://"+spec.Labels[runtime.LabelRecipe] {
			packageFound = true
		}
		if resource.Kind == downloads.ResourceImage && (resource.IndexDigest == spec.ImageDigest || resource.ManifestDigest == spec.ImageDigest) {
			spec.Image = resource.Source.Reference
			// Docker can cache a child descriptor without a separately addressable
			// repository reference. Launch the verified index; resource checks
			// still enforce the exact child manifest and platform.
			spec.ImageDigest = resource.IndexDigest
			spec.ImagePlatform = resource.Platform
			imageFound = true
		}
	}
	if spec.Upstream != nil && !packageFound {
		return fmt.Errorf("upstream recipe package has no reviewed resource on node %s", nodeID)
	}
	if spec.Upstream == nil && !imageFound {
		return fmt.Errorf("container image has no reviewed platform manifest on node %s", nodeID)
	}
	spec.AcquisitionPolicy = AcquisitionRequireExisting
	return nil
}
