package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func decodeInstallationUpdate(raw []byte) ([]recipe.InstallationUpdateSpec, error) {
	if len(raw) == 0 || len(raw) > recipe.MaxInstallationUpdateBytes {
		return nil, fmt.Errorf("recipe.update_specs_invalid: bounded JSON array required")
	}
	var inputs []recipe.InstallationUpdateSpec
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&inputs) != nil || decoder.Decode(new(any)) != io.EOF || inputs == nil || len(inputs) > recipe.MaxInstallationUpdateSpecs {
		return nil, fmt.Errorf("recipe.update_specs_invalid")
	}
	return inputs, nil
}

func (a *Agent) handleRecipeUpdate(ctx context.Context, command *agentv1.ArtifactCommand) {
	inputs, err := decodeInstallationUpdate(command.GetUpstreamSpecs())
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	resource, err := a.legacyResource(command.GetArtifactIdentity(), "")
	if err == nil && (resource.Kind != downloads.ResourceRecipe || command.GetCacheRoot() != "" || command.GetBearerToken() != "") {
		err = fmt.Errorf("recipe.update_identity_invalid")
	}
	if err == nil {
		_, err = a.authorizeDownload(ctx, resource)
	}
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	sum := sha256.Sum256(command.GetUpstreamSpecs())
	commandCtx, attempt, fresh, err := a.beginAcquisition(ctx, command.GetCommandId(), resource.Identity, resource.Destination, fmt.Sprintf("recipe-update:%x", sum))
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	if !fresh {
		select {
		case <-attempt.done:
		case <-ctx.Done():
			return
		}
		if attempt.Error != "" {
			err = fmt.Errorf("%s", attempt.Error)
		}
		a.downloadResult(command.GetCommandId(), attempt.Output, err)
		return
	}
	// This is deliberately not DOWNLOAD_OP_FETCH: even cached package bytes must
	// be authenticated before target-bound source preparation completes the job.
	err = a.fetchRecipePackage(commandCtx, resource.Identity)
	if err == nil {
		a.sendPlacement(placementCandidate{Identity: resource.Identity, Path: resource.Destination, State: "valid", Size: regularTreeSize(commandCtx, resource.Destination)})
		err = a.prepareRecipeUpdate(commandCtx, resource, inputs)
	}
	if err == nil {
		err = commandCtx.Err()
	}
	a.finishAcquisition(attempt, nil, err)
	if attempt.Error != "" {
		err = fmt.Errorf("%s", attempt.Error)
	}
	a.downloadResult(command.GetCommandId(), nil, err)
}

func (a *Agent) prepareRecipeUpdate(ctx context.Context, resource downloads.ResourceSpec, inputs []recipe.InstallationUpdateSpec) error {
	packed, err := recipe.ReadLayout(resource.Destination)
	if err != nil {
		return err
	}
	if packed.ManifestDigest != resource.Source.Digest {
		return fmt.Errorf("recipe.update_package_mismatch")
	}
	manifest, err := recipe.Parse(packed.ConfigJSON)
	if err != nil {
		return err
	}
	specs, err := validateInstallationUpdate(manifest, resource.Source.Digest, inputs)
	if err != nil {
		return err
	}
	if manifest.Metadata.Source != nil {
		retained, err := runtime.RetainedUpstreamSpecs(a.rt, manifest.Metadata.Source.URL, manifest.Metadata.Source.Path)
		if err != nil {
			return err
		}
		for _, old := range retained {
			if !slices.ContainsFunc(specs, func(spec runtime.ContainerSpec) bool {
				return installationOwnershipKey(spec) == installationOwnershipKey(old)
			}) {
				return fmt.Errorf("recipe.update_history_missing: retained deployment %s rank %s has no reconstructed target configuration; original installation preserved", old.Labels[runtime.LabelDeployment], old.Labels[runtime.LabelRank])
			}
		}
	}
	for i := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := runtime.PrepareUpstreamSource(ctx, a.rt, &specs[i]); err != nil {
			return fmt.Errorf("recipe.update_source_prepare_failed: rank %s: %w", specs[i].Labels[runtime.LabelRank], err)
		}
	}
	return nil
}

func installationOwnershipKey(spec runtime.ContainerSpec) string {
	if spec.Upstream == nil {
		return ""
	}
	names := append(append([]string{}, spec.Upstream.Containers...), spec.Upstream.AuxiliaryContainers...)
	slices.Sort(names)
	return spec.Labels[runtime.LabelRank] + "\x00" + strings.Join(names, "\x00")
}

func validateInstallationUpdate(manifest *recipe.Manifest, digest string, inputs []recipe.InstallationUpdateSpec) ([]runtime.ContainerSpec, error) {
	specs := make([]runtime.ContainerSpec, 0, len(inputs))
	seen := map[string]bool{}
	for _, input := range inputs {
		var spec runtime.ContainerSpec
		decoder := json.NewDecoder(bytes.NewReader(input.Spec))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&spec) != nil || decoder.Decode(new(any)) != io.EOF || spec.Upstream == nil {
			return nil, fmt.Errorf("recipe.update_spec_invalid")
		}
		if spec.Labels[runtime.LabelRecipe] != digest || spec.Labels[runtime.LabelRank] != strconv.Itoa(input.Rank) {
			return nil, fmt.Errorf("recipe.update_spec_identity_mismatch")
		}
		if err := runtime.ValidateManagedSpec(&spec); err != nil {
			return nil, err
		}
		// No image, command, mount, host-preparation or other lifecycle contract
		// can be smuggled beside the package-authenticated upstream definition.
		allowed := runtime.ContainerSpec{Name: spec.Name, Labels: spec.Labels, Env: spec.Env, Upstream: spec.Upstream}
		if !reflect.DeepEqual(spec, allowed) {
			return nil, fmt.Errorf("recipe.update_executable_contract_invalid")
		}
		execution, environment, configuration, err := recipe.RenderInstallationUpstream(manifest, input)
		if err != nil {
			return nil, err
		}
		source := manifest.Metadata.Source
		expected := &runtime.UpstreamSpec{SourceURL: source.URL, Revision: source.Revision, SourcePath: source.Path, Install: execution.Install, Start: execution.Start, Stop: execution.Stop, Containers: execution.Containers, AuxiliaryContainers: execution.AuxiliaryContainers, ObserveOnly: execution.CoordinatorRank != nil && *execution.CoordinatorRank != input.Rank, EnvFile: execution.EnvFile, EnvTemplate: execution.EnvTemplate, EnvFormat: execution.EnvFormat, LogFile: execution.LogFile, Approved: true, Configuration: configuration}
		actualJSON, _ := json.Marshal(spec.Upstream)
		expectedJSON, _ := json.Marshal(expected)
		if !bytes.Equal(actualJSON, expectedJSON) {
			return nil, fmt.Errorf("recipe.update_upstream_contract_mismatch")
		}
		keys := make([]string, 0, len(environment))
		for key := range environment {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		var env []string
		for _, key := range keys {
			env = append(env, key+"="+environment[key])
		}
		if !slices.Equal(spec.Env, env) {
			return nil, fmt.Errorf("recipe.update_environment_mismatch")
		}
		key := installationOwnershipKey(spec)
		if seen[key] {
			return nil, fmt.Errorf("recipe.update_duplicate_installation")
		}
		seen[key] = true
		specs = append(specs, spec)
	}
	return specs, nil
}
