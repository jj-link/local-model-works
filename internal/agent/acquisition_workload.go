package agent

import (
	"context"
	"fmt"

	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/runtime"
)

func (a *Agent) ensureWorkloadImage(ctx context.Context, spec *runtime.ContainerSpec) error {
	if spec.AcquisitionPolicy == "require-existing" {
		_, err := a.rt.InspectImage(ctx, runtime.ImageRef(spec), spec.ImagePlatform)
		if err != nil {
			return fmt.Errorf("download.recheck_required: workload image is not present: %w", err)
		}
		return nil
	}
	if spec.AcquisitionPolicy != "" && spec.AcquisitionPolicy != "download-missing" {
		return fmt.Errorf("download.acquisition_policy_invalid")
	}
	return a.rt.Pull(ctx, &runtime.PullSpec{Reference: runtime.ImageRef(spec), Platform: spec.ImagePlatform})
}
func (a *Agent) checkExistingResources(ctx context.Context, spec *runtime.ContainerSpec) error {
	if spec.AcquisitionPolicy != "require-existing" {
		return nil
	}
	if len(spec.AcquisitionResources) == 0 {
		return fmt.Errorf("download.recheck_required: exact resource inventory missing")
	}
	for _, resource := range spec.AcquisitionResources {
		if err := resource.Validate(); err != nil {
			return err
		}
		root, err := a.authorizeDownload(ctx, resource)
		if err != nil {
			return err
		}
		observation, err := a.inspectDownload(ctx, resource, root, nil, func(artifactDownloadProgress) {})
		if err != nil {
			return fmt.Errorf("download.recheck_required: %w", err)
		}
		if observation.State != downloads.ResourceAvailable {
			return fmt.Errorf("download.recheck_required: %s is not available", resource.Identity)
		}
	}
	return a.ensureWorkloadImage(ctx, spec)
}
