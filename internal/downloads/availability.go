package downloads

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/recipe"
)

func (s *Service) Availability(ctx context.Context, req AvailabilityRequest) (*Availability, error) {
	targets := req.Targets
	if len(targets) == 0 {
		nodes, err := s.q.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			targets = append(targets, Target{NodeID: n.ID})
		}
	}
	if len(targets) == 0 {
		return &Availability{RecipeDigest: req.RecipeDigest, Variants: map[string]string{}, Devices: []DeviceAvailability{}, Diagnostics: []diag.Diagnostic{}}, nil
	}
	plan, err := s.plan(ctx, PlanRequest{RecipeDigest: req.RecipeDigest, Targets: targets, WorkloadIndex: req.WorkloadIndex, Variants: req.Variants, LaunchProfileID: req.LaunchProfileID}, req.Refresh)
	if err != nil {
		return nil, err
	}
	out := &Availability{RecipeDigest: plan.RecipeDigest, WorkloadIndex: plan.WorkloadIndex, Variants: plan.Variants, Devices: []DeviceAvailability{}, Diagnostics: plan.Diagnostics}
	attempts, err := s.List(ctx, req.RecipeDigest)
	if err != nil {
		return nil, err
	}
	deployments, err := s.q.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}
	related := map[string]bool{req.RecipeDigest: true}
	if membership, e := s.q.GetRecipeRepositoryVersionByDigest(ctx, req.RecipeDigest); e == nil {
		versions, e := s.q.ListRecipeRepositoryVersions(ctx, membership.RepositoryID)
		if e != nil {
			return nil, e
		}
		for _, version := range versions {
			related[version.RecipeDigest] = true
		}
	}
	saved, err := s.q.GetRecipe(ctx, req.RecipeDigest)
	if err != nil {
		return nil, err
	}
	var manifest recipe.Manifest
	if err = json.Unmarshal([]byte(saved.Manifest), &manifest); err != nil {
		return nil, err
	}
	supported := map[string]map[string]string{}
	for _, artifact := range manifest.Artifacts {
		for _, variant := range artifact.Variants {
			identity, e := artifactidentity.Canonical(variant.Source.Type, variant.Source.Identity, variant.Source.Revision, variant.Source.Digest)
			if e != nil {
				continue
			}
			choices := map[string]string{}
			for name, value := range plan.Variants {
				choices[name] = value
			}
			choices[artifact.Name] = variant.Name
			supported[identity] = choices
		}
	}
	for _, target := range plan.Targets {
		n, err := s.q.GetNode(ctx, target.NodeID)
		if err != nil {
			return nil, err
		}
		device := DeviceAvailability{NodeID: n.ID, NodeName: n.DisplayName, Online: s.nodes.Online(n.ID), Resources: []AvailableResource{}, OtherVersions: []OtherVersion{}, ActiveRunIDs: []string{}, RunningDeployments: []RunningDeployment{}, RuntimeDiagnostics: []diag.Diagnostic{}, Diagnostics: []diag.Diagnostic{}}
		identities := map[string]bool{}
		for _, r := range plan.Resources {
			if r.NodeID != n.ID {
				continue
			}
			verification := r.Verification
			if !device.Online {
				verification.Stale = true
			}
			device.Resources = append(device.Resources, AvailableResource{Key: r.Key, Kind: r.Kind, Identity: r.Identity, Destination: r.Destination, Platform: r.Platform, IndexDigest: r.IndexDigest, ManifestDigest: r.ManifestDigest, Required: r.Required, Verification: verification})
			identities[r.Identity] = true
			if verification.VerifiedAt != nil && (device.LastCheckedAt == nil || verification.VerifiedAt.After(*device.LastCheckedAt)) {
				t := *verification.VerifiedAt
				device.LastCheckedAt = &t
			}
		}
		placements, err := s.q.ListPlacementsOnNode(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range placements {
			art, err := s.q.GetArtifact(ctx, p.ArtifactID)
			if err != nil {
				return nil, err
			}
			key := art.Identity + "\n" + p.Path
			if identities[art.Identity] || seen[key] {
				continue
			}
			seen[key] = true
			relevant := supported[art.Identity] != nil
			if strings.HasPrefix(art.Identity, "recipe://") {
				relevant = related[strings.TrimPrefix(art.Identity, "recipe://")]
			}
			for _, r := range plan.Resources {
				if r.Source.Type == SourceHuggingFace && strings.HasPrefix(art.Identity, "hf://"+r.Source.Reference+"@") {
					relevant = true
				}
			}
			if !relevant {
				continue
			}
			state := ResourceInvalid
			if p.State == "valid" {
				state = ResourceAvailable
			}
			verification := Verification{State: state, Stale: true}
			if p.VerifiedAt.Valid {
				t, e := time.Parse(time.RFC3339Nano, p.VerifiedAt.String)
				if e == nil {
					verification.VerifiedAt = &t
				}
			}
			kind := ResourceArtifact
			if art.Kind == "recipe" {
				kind = ResourceRecipe
			}
			device.OtherVersions = append(device.OtherVersions, OtherVersion{Kind: kind, Identity: art.Identity, Path: p.Path, Verification: verification, SupportedVariants: supported[art.Identity]})
		}
		for _, a := range attempts {
			switch a.State {
			case "queued", "running", "verifying", "cancelling":
			default:
				continue
			}
			if a.Input.Plan.WorkloadIndex != plan.WorkloadIndex || encoded(a.Input.Plan.Variants) != encoded(plan.Variants) {
				continue
			}
			for _, t := range a.Input.Plan.Targets {
				if t.NodeID == n.ID {
					device.ActiveRunIDs = append(device.ActiveRunIDs, a.RunID)
					break
				}
			}
		}
		// Placement entries, not package cache state, establish where workloads run.
		for _, d := range deployments {
			if d.DesiredState != "running" || !related[d.RecipeDigest] {
				continue
			}
			var placement struct {
				Entries []struct {
					NodeID string `json:"node_id"`
					Rank   int32  `json:"rank"`
				} `json:"entries"`
				Ranks         map[string]int32  `json:"ranks"`
				Variants      map[string]string `json:"variants"`
				WorkloadIndex int               `json:"workload_index"`
			}
			if json.Unmarshal([]byte(d.Placement), &placement) != nil {
				continue
			}
			ranks := []int32{}
			for _, entry := range placement.Entries {
				if entry.NodeID == n.ID {
					ranks = append(ranks, entry.Rank)
				}
			}
			if len(placement.Entries) == 0 {
				if rank, ok := placement.Ranks[n.ID]; ok {
					ranks = append(ranks, rank)
				}
			}
			for _, rank := range ranks {
				device.RunningDeployments = append(device.RunningDeployments, RunningDeployment{DeploymentID: d.ID, RecipeDigest: d.RecipeDigest, WorkloadIndex: placement.WorkloadIndex, Variants: placement.Variants, Rank: rank, State: d.ObservedState})
			}
		}
		if !runtimeMatch(manifest.Workloads[plan.WorkloadIndex], nodeReport(n)) {
			device.RuntimeDiagnostics = append(device.RuntimeDiagnostics, diag.Warning("download.runtime_incompatible", "This device can store files, but does not match the selected runtime hardware"))
		}
		if !device.Online {
			device.Diagnostics = append(device.Diagnostics, diag.Warning("download.device_offline", "Last checked observations are retained; device is offline"))
		}
		sort.Slice(device.OtherVersions, func(i, j int) bool {
			if device.OtherVersions[i].Identity == device.OtherVersions[j].Identity {
				return device.OtherVersions[i].Path < device.OtherVersions[j].Path
			}
			return device.OtherVersions[i].Identity < device.OtherVersions[j].Identity
		})
		out.Devices = append(out.Devices, device)
	}
	return out, nil
}
