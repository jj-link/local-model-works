package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
)

type nodeInventory struct {
	inventory.Inventory
	Arch             string   `json:"arch"`
	ProtocolFeatures []string `json:"protocol_features"`
	DownloadRoots    struct {
		RecipeRoot string `json:"recipe_root"`
		ImageRoot  string `json:"image_root"`
		Platform   string `json:"platform"`
	} `json:"download_roots"`
}

func nodeReport(n db.Node) nodeInventory {
	var v nodeInventory
	_ = json.Unmarshal([]byte(n.Inventory.String), &v)
	return v
}
func hasFeature(v nodeInventory) bool {
	for _, f := range v.ProtocolFeatures {
		if f == ProtocolFeature {
			return true
		}
	}
	return false
}
func (s *Service) Plan(ctx context.Context, req PlanRequest) (*Plan, error) {
	return s.plan(ctx, req, true)
}
func (s *Service) plan(ctx context.Context, req PlanRequest, refresh bool) (*Plan, error) {
	if len(req.Targets) == 0 || len(req.Targets) > MaxTargets {
		return nil, failure("download.targets_required", "Choose explicit enrolled device IDs", 422)
	}
	if req.ResumeRunID != "" && req.Credentials == nil {
		prior, err := s.attempt(ctx, req.RecipeDigest, req.ResumeRunID)
		if err != nil {
			return nil, err
		}
		req.Credentials = prior.Input.Credentials
	}
	if len(req.Credentials) > MaxCredentials {
		return nil, failure("download.credentials_invalid", "Too many credential selections", 422)
	}
	if req.LaunchProfileID != "" {
		if len(req.Variants) > 0 || req.WorkloadIndex != nil {
			return nil, failure("download.profile_conflict", "Choose a saved launch profile or explicit runtime and variants", 422)
		}
		p, err := s.q.GetLaunchProfile(ctx, req.LaunchProfileID)
		if err != nil {
			return nil, err
		}
		if p.RecipeDigest != req.RecipeDigest {
			return nil, failure("download.profile_mismatch", "Launch profile belongs to another saved recipe", 422)
		}
		if err = json.Unmarshal([]byte(p.Variants), &req.Variants); err != nil {
			return nil, err
		}
	}
	row, err := s.q.GetRecipe(ctx, req.RecipeDigest)
	if err != nil {
		return nil, err
	}
	var manifest recipe.Manifest
	if err = json.Unmarshal([]byte(row.Manifest), &manifest); err != nil {
		return nil, err
	}
	plan := &Plan{RecipeDigest: req.RecipeDigest, Variants: map[string]string{}, Targets: append([]Target(nil), req.Targets...), Resources: []Resource{}, Storage: []Storage{}, Diagnostics: []diag.Diagnostic{}, Ready: true}
	block := func(code, msg string) {
		plan.Diagnostics = append(plan.Diagnostics, diag.Error(code, msg))
		plan.Ready = false
	}
	nodes := make(map[string]db.Node, len(req.Targets))
	reports := make(map[string]nodeInventory, len(req.Targets))
	for i, t := range plan.Targets {
		if t.NodeID == "" {
			return nil, failure("download.targets_required", "Device ID cannot be empty", 422)
		}
		if _, ok := nodes[t.NodeID]; ok {
			return nil, failure("download.targets_duplicate", "Device IDs must be unique", 422)
		}
		n, e := s.q.GetNode(ctx, t.NodeID)
		if e != nil {
			return nil, failure("download.node_unknown", "Selected device is not enrolled", 422)
		}
		nodes[t.NodeID] = n
		inv := nodeReport(n)
		reports[t.NodeID] = inv
		if !s.nodes.Online(t.NodeID) {
			block("download.node_offline", n.DisplayName+" is offline")
		}
		if !hasFeature(inv) {
			block("download.agent_upgrade_required", n.DisplayName+" needs an agent with downloads-v1")
		}
		found := false
		for _, root := range inv.CacheRoots {
			if root.Writable && (t.CacheRoot == "" || t.CacheRoot == root.Path) {
				plan.Targets[i].CacheRoot = root.Path
				found = true
				break
			}
		}
		if !found {
			block("download.storage_unavailable", n.DisplayName+" has no selected reported writable cache root")
		}
	}
	sort.Slice(plan.Targets, func(i, j int) bool { return plan.Targets[i].NodeID < plan.Targets[j].NodeID })
	if req.WorkloadIndex != nil {
		plan.WorkloadIndex = *req.WorkloadIndex
		if plan.WorkloadIndex < 0 || plan.WorkloadIndex >= len(manifest.Workloads) {
			return nil, failure("download.runtime_invalid", "Selected runtime does not exist", 422)
		}
	} else if len(manifest.Workloads) == 1 {
		plan.WorkloadIndex = 0
	} else {
		matches := []int{}
		for i, w := range manifest.Workloads {
			ok := true
			for _, inv := range reports {
				if !runtimeMatch(w, inv) {
					ok = false
					break
				}
			}
			if ok {
				matches = append(matches, i)
			}
		}
		if len(matches) != 1 {
			return nil, failure("download.runtime_selection_required", "Choose the runtime whose files should be downloaded; device occupancy is not used", 422)
		}
		plan.WorkloadIndex = matches[0]
	}
	for name := range req.Variants {
		found := false
		for _, a := range manifest.Artifacts {
			if a.Name == name && len(a.Variants) > 0 {
				found = true
			}
		}
		if !found {
			return nil, failure("download.variant_invalid", "Unknown variant artifact: "+name, 422)
		}
	}
	for _, a := range manifest.Artifacts {
		if len(a.Variants) > 0 {
			v := req.Variants[a.Name]
			if v == "" {
				v = a.DefaultVariant
			}
			if _, e := a.EffectiveSource(v); e != nil {
				return nil, e
			}
			plan.Variants[a.Name] = v
		}
	}
	if req.ResumeRunID != "" {
		prior, e := s.attempt(ctx, req.RecipeDigest, req.ResumeRunID)
		if e != nil {
			return nil, e
		}
		if prior.State != "failed" && prior.State != "cancelled" && prior.State != "interrupted" {
			return nil, failure("download.resume_state", "This download attempt cannot be resumed", 409)
		}
		p := prior.Input.Plan
		if p.RecipeDigest != plan.RecipeDigest || p.WorkloadIndex != plan.WorkloadIndex || !maps.Equal(p.Variants, plan.Variants) || encoded(p.Targets) != encoded(plan.Targets) {
			return nil, failure("download.resume_choices_changed", "Resume must retain the exact saved content, runtime, variants and devices", 412)
		}
	}
	selected := map[string]bool{}
	for _, c := range req.Credentials {
		if c.Resource == "" || c.Host == "" || c.SecretID == "" || selected[c.Resource] {
			return nil, failure("download.credentials_invalid", "Select one exact host and secret ID per resource", 422)
		}
		selected[c.Resource] = true
	}
	seen := map[string]bool{}
	observations := map[string]*StorageObservation{}
	for _, target := range plan.Targets {
		inv := reports[target.NodeID]
		specs := []ResourceSpec{}
		if inv.DownloadRoots.RecipeRoot == "" {
			block("download.package_storage_unknown", "Device did not report its recipe package root")
		} else {
			specs = append(specs, ResourceSpec{Kind: ResourceRecipe, Identity: "recipe://" + req.RecipeDigest, Source: SourceSpec{Type: SourceRecipe, Digest: req.RecipeDigest}, Destination: path.Join(inv.DownloadRoots.RecipeRoot, strings.TrimPrefix(req.RecipeDigest, "sha256:"))})
		}
		for _, artifact := range manifest.Artifacts {
			if target.CacheRoot == "" {
				continue
			}
			spec, e := selectedArtifact(artifact, plan.Variants[artifact.Name], target.CacheRoot)
			if e != nil {
				block("download.identity_unverified", e.Error())
				continue
			}
			specs = append(specs, spec)
		}
		var images []recipe.Image
		if manifest.Workloads[plan.WorkloadIndex].Upstream == nil {
			images = append(images, manifest.Workloads[plan.WorkloadIndex].Image)
		}
		if manifest.Prepare != nil {
			images = append(images, manifest.Prepare.Image)
		}
		if manifest.Verify != nil {
			images = append(images, manifest.Verify.Image)
		}
		if manifest.Workloads[plan.WorkloadIndex].HostPreparation != nil {
			ref, digest, _ := strings.Cut(HostPreparationImage, "@")
			images = append(images, recipe.Image{Reference: ref, Digest: digest})
		}
		imageKeys := map[string]bool{}
		for _, image := range images {
			key := normalizeImage(image.Reference) + "@" + image.Digest
			if imageKeys[key] {
				continue
			}
			imageKeys[key] = true
			spec, e := s.imageSpec(ctx, image, inv, target.NodeID, req.Credentials)
			if e != nil {
				block("download.image_unavailable", e.Error())
				continue
			}
			specs = append(specs, spec)
		}
		for _, spec := range specs {
			if e := spec.Validate(); e != nil {
				block("download.resource_invalid", e.Error())
				continue
			}
			r := Resource{ResourceSpec: spec, NodeID: target.NodeID, Action: ActionDownloadOrigin, Required: true, Verification: Verification{State: ResourceUnknown, Stale: true}}
			for _, c := range req.Credentials {
				if c.Resource == r.Identity {
					r.CredentialID = c.SecretID
				}
			}
			r.Key = resourceKey(r)
			if seen[r.Key] {
				continue
			}
			seen[r.Key] = true
			if refresh && s.nodes.Online(target.NodeID) && hasFeature(inv) {
				out, e := s.inspectCandidates(ctx, &r, target, req.Credentials)
				if e != nil {
					block("download.inspect_failed", nodes[target.NodeID].DisplayName+": "+e.Error())
				} else {
					r.Verification = Verification{State: out.State, VerifiedAt: out.VerifiedAt}
					if out.SizeBytes != nil {
						r.SizeBytes = out.SizeBytes
					}
					r.BytesRemaining = out.BytesRemaining
					observations[r.Key] = out.Storage
					if e = s.recordObservation(ctx, r, out); e != nil {
						block("download.observation_failed", "Cannot persist verified device observation")
					}
				}
			} else {
				s.storedObservation(ctx, &r)
			}
			switch r.Verification.State {
			case ResourceAvailable:
				if !r.Verification.Stale {
					r.Action = ActionReuse
				}
			case ResourcePartial:
				r.Action = ActionDownloadOrigin
			default:
				if r.Kind == ResourceArtifact {
					peer, p, e := s.selectPeer(ctx, &r, req.Credentials, refresh)
					if e == nil && peer != "" {
						r.Action = ActionPeerCopy
						r.SourceNode = peer
						r.SourcePath = p
					}
				}
			}
			if r.Action != ActionReuse && r.Action != ActionPeerCopy {
				if r.Source.Type == SourceLocal || (r.Source.Type == SourceFile && r.Source.URL == "") {
					block("download.source_unavailable", "No retrievable origin or verified peer for "+r.Identity)
				}
				if r.Source.Type == SourceOCI && r.Kind == ResourceArtifact {
					block("download.artifact_layout_unsupported", "OCI data layout must be verified as a supported artifact before acquisition: "+r.Identity)
				}
			}
			if r.BytesTotal == nil {
				r.BytesTotal = r.SizeBytes
			}
			if r.Action == ActionReuse {
				zero := int64(0)
				r.BytesRemaining = &zero
			} else if r.BytesRemaining == nil {
				r.BytesRemaining = r.SizeBytes
			}
			if r.Kind == ResourceImage && r.Action != ActionReuse && r.SizeBytes == nil {
				block("download.image_storage_unknown", "Cannot establish expanded engine storage for "+r.Identity+"; compressed registry sizes do not prove sufficient device capacity")
			}
			if r.Action != ActionReuse {
				if lock, e := s.q.GetDestinationLock(ctx, db.GetDestinationLockParams{NodeID: r.NodeID, Destination: r.Destination}); e == nil && lock.State != "quiesced" {
					block("download.destination_busy", "An acquisition still owns "+r.Destination+"; wait for device cancellation acknowledgement")
				}
			}
			// Changing destination to a verified local candidate also changes lock and review identity.
			oldKey := r.Key
			r.Key = resourceKey(r)
			observations[r.Key] = observations[oldKey]
			plan.Resources = append(plan.Resources, r)
		}
	}
	if len(plan.Resources) > MaxResources {
		return nil, failure("download.resources_limit", "Too many distinct required resources", 422)
	}
	for _, c := range req.Credentials {
		found := false
		for _, r := range plan.Resources {
			if r.Identity == c.Resource {
				found = true
			}
		}
		if !found {
			return nil, failure("download.credentials_invalid", "Credential selection does not match a selected resource", 422)
		}
	}
	sort.Slice(plan.Resources, func(i, j int) bool { return plan.Resources[i].Key < plan.Resources[j].Key })
	if req.ResumeRunID != "" {
		prior, e := s.attempt(ctx, req.RecipeDigest, req.ResumeRunID)
		if e != nil {
			return nil, e
		}
		frozen := map[string]Resource{}
		for _, r := range prior.Input.Plan.Resources {
			frozen[r.Key] = r
		}
		if len(frozen) != len(plan.Resources) {
			return nil, failure("download.resume_choices_changed", "Resume cannot change the frozen resource set", 412)
		}
		for _, r := range plan.Resources {
			before, ok := frozen[r.Key]
			if !ok || before.ManifestDigest != r.ManifestDigest || before.IndexDigest != r.IndexDigest {
				return nil, failure("download.resume_choices_changed", "Resume cannot change content, destinations or image platforms", 412)
			}
		}
	}
	plan.Storage = storagePlan(plan.Resources, observations)
	for _, st := range plan.Storage {
		if st.Sufficient == nil {
			block("download.storage_unknown", "Cannot establish required size and current writable capacity at "+st.Destination)
		} else if !*st.Sufficient {
			block("download.storage_insufficient", "Not enough writable storage including staging and 5 GiB reserve at "+st.Destination)
		}
	}
	plan.PlanDigest = planHash(plan)
	return plan, nil
}
func runtimeMatch(w recipe.Workload, inv nodeInventory) bool {
	if w.Match == nil || w.Match.Accelerator == nil {
		return true
	}
	m := w.Match.Accelerator
	for _, a := range inv.Accelerators {
		if m.Vendor != "" && m.Vendor != a.Vendor {
			continue
		}
		if len(m.Architectures) > 0 {
			ok := false
			for _, arch := range m.Architectures {
				if arch == a.Architecture {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		all := true
		for _, f := range m.Features {
			ok := false
			for _, have := range a.Features {
				if f == have {
					ok = true
				}
			}
			if !ok {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}
func resourceKey(r Resource) string {
	sum := sha256.Sum256([]byte(string(r.Kind) + "\n" + r.Identity + "\n" + r.NodeID + "\n" + r.Destination + "\n" + r.Platform))
	return hex.EncodeToString(sum[:])
}
func planHash(p *Plan) string {
	type binding struct {
		Spec                                             ResourceSpec `json:"resource"`
		Node, Action, SourceNode, SourcePath, Credential string
		Required                                         bool
	}
	rs := make([]binding, 0, len(p.Resources))
	for _, r := range p.Resources {
		spec := r.ResourceSpec
		spec.SizeBytes = nil
		rs = append(rs, binding{spec, r.NodeID, string(r.Action), r.SourceNode, r.SourcePath, r.CredentialID, r.Required})
	}
	sum := sha256.Sum256([]byte(encoded(struct {
		Recipe    string
		Workload  int
		Variants  map[string]string
		Targets   []Target
		Resources []binding
	}{p.RecipeDigest, p.WorkloadIndex, p.Variants, p.Targets, rs})))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func (s *Service) inspectCandidates(ctx context.Context, r *Resource, target Target, creds []CredentialSelection) (CommandOutput, error) {
	paths := []string{r.Destination}
	roots := []string{target.CacheRoot}
	if node, e := s.q.GetNode(ctx, r.NodeID); e == nil {
		for _, root := range nodeReport(node).CacheRoots {
			if root.Path != target.CacheRoot {
				roots = append(roots, root.Path)
			}
		}
	}
	if r.Kind == ResourceArtifact {
		if art, e := s.q.GetArtifactByIdentity(ctx, r.Identity); e == nil {
			rows, e := s.q.ListPlacements(ctx, art.ID)
			if e != nil {
				return CommandOutput{}, e
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
			for _, placement := range rows {
				if placement.NodeID != r.NodeID || placement.Path == r.Destination {
					continue
				}
				for _, root := range roots {
					if root != "" && (placement.Path == root || strings.HasPrefix(placement.Path, root+"/")) {
						paths = append(paths, placement.Path)
						break
					}
				}
			}
		}
	}
	if r.Source.Type == SourceHuggingFace {
		for _, root := range roots {
			if root == "" {
				continue
			}
			model := "models--" + strings.ReplaceAll(r.Source.Reference, "/", "--")
			paths = append(paths, path.Join(root, "hub", model), path.Join(root, model))
		}
	}
	seen := map[string]bool{}
	var first CommandOutput
	var firstErr error
	for i, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		spec := r.ResourceSpec
		spec.Destination = p
		out, e := s.inspect(ctx, r.NodeID, spec, creds)
		if i == 0 {
			first, firstErr = out, e
		}
		if e == nil && out.State == ResourceAvailable && out.VerifiedAt != nil {
			r.Destination = p
			return out, nil
		}
		if e == nil {
			candidate := *r
			candidate.Destination = p
			s.recordObservation(ctx, candidate, out)
		}
	}
	return first, firstErr
}
func (s *Service) storedObservation(ctx context.Context, r *Resource) {
	art, e := s.q.GetArtifactByIdentity(ctx, r.Identity)
	if e != nil {
		return
	}
	rows, e := s.q.ListPlacements(ctx, art.ID)
	if e != nil {
		return
	}
	for _, p := range rows {
		if p.NodeID != r.NodeID || p.Path != r.Destination {
			continue
		}
		if p.VerifiedAt.Valid {
			t, e := time.Parse(time.RFC3339Nano, p.VerifiedAt.String)
			if e == nil {
				r.Verification.VerifiedAt = &t
			}
		}
		switch p.State {
		case "valid":
			r.Verification.State = ResourceAvailable
		case "invalid":
			r.Verification.State = ResourceInvalid
		default:
			r.Verification.State = ResourcePartial
		}
		r.Verification.Stale = true
		return
	}
}
func (s *Service) selectPeer(ctx context.Context, r *Resource, creds []CredentialSelection, refresh bool) (string, string, error) {
	art, err := s.q.GetArtifactByIdentity(ctx, r.Identity)
	if err != nil {
		return "", "", err
	}
	rows, err := s.q.ListPlacements(ctx, art.ID)
	if err != nil {
		return "", "", err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].NodeID == rows[j].NodeID {
			return rows[i].Path < rows[j].Path
		}
		return rows[i].NodeID < rows[j].NodeID
	})
	for _, placement := range rows {
		if placement.NodeID == r.NodeID || placement.State != "valid" || !s.nodes.Online(placement.NodeID) {
			continue
		}
		node, err := s.q.GetNode(ctx, placement.NodeID)
		if err != nil {
			continue
		}
		report := nodeReport(node)
		if !hasFeature(report) || !validPeerAddress(report.PeerListen) {
			continue
		}
		if refresh {
			spec := r.ResourceSpec
			spec.Destination = placement.Path
			out, err := s.inspect(ctx, placement.NodeID, spec, creds)
			if err != nil || out.State != ResourceAvailable || out.VerifiedAt == nil || out.TreeDigest == "" || out.TreeSizeBytes == nil {
				continue
			}
			r.BytesTotal = out.TreeSizeBytes
			r.BytesRemaining = out.TreeSizeBytes
		}
		return placement.NodeID, placement.Path, nil
	}
	return "", "", nil
}
func storagePlan(resources []Resource, observations map[string]*StorageObservation) []Storage {
	groups := map[string]*Storage{}
	unknown := map[string]bool{}
	for _, r := range resources {
		obs := observations[r.Key]
		fs := r.Destination
		if obs != nil && obs.Filesystem != "" {
			fs = obs.Filesystem
		}
		key := r.NodeID + "\n" + fs
		st := groups[key]
		if st == nil {
			zero, stage := int64(0), int64(0)
			st = &Storage{NodeID: r.NodeID, Filesystem: fs, Destination: r.Destination, ResourceKeys: []string{}, RequiredBytes: &zero, StagingBytes: &stage, ReserveBytes: StorageReserveBytes}
			groups[key] = st
		}
		st.ResourceKeys = append(st.ResourceKeys, r.Key)
		if obs != nil {
			if obs.AvailableBytes != nil && (st.AvailableBytes == nil || *obs.AvailableBytes < *st.AvailableBytes) {
				v := *obs.AvailableBytes
				st.AvailableBytes = &v
			}
			st.TotalBytes = obs.TotalBytes
		}
		if r.Action == ActionReuse {
			continue
		}
		if r.BytesRemaining == nil {
			unknown[key] = true
			continue
		}
		if *r.BytesRemaining < 0 || *r.BytesRemaining > (1<<62)-*st.RequiredBytes {
			unknown[key] = true
			continue
		}
		*st.RequiredBytes += *r.BytesRemaining
		*st.StagingBytes += *r.BytesRemaining
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Storage, 0, len(keys))
	for _, key := range keys {
		st := groups[key]
		if unknown[key] {
			st.RequiredBytes = nil
			st.StagingBytes = nil
		} else if st.AvailableBytes != nil {
			need := *st.RequiredBytes + *st.StagingBytes
			if need <= int64(^uint64(0)>>1)-st.ReserveBytes {
				ok := *st.AvailableBytes >= need+st.ReserveBytes
				st.Sufficient = &ok
			}
		}
		out = append(out, *st)
	}
	return out
}
func (s *Service) recordObservation(ctx context.Context, r Resource, out CommandOutput) error {
	if r.Kind == ResourceImage {
		return nil
	}
	art, err := s.q.GetArtifactByIdentity(ctx, r.Identity)
	if err != nil {
		if out.State != ResourceAvailable || out.VerifiedAt == nil {
			return nil
		}
		facts, parseErr := artifactidentity.Parse(r.Identity)
		if parseErr != nil {
			return parseErr
		}
		kind := facts.Kind
		if err = s.q.CreateArtifact(ctx, db.CreateArtifactParams{ID: newID(), Kind: kind, Identity: r.Identity, Revision: ns(r.Source.Revision), Digest: ns(r.Source.Digest), Metadata: encoded(map[string]any{"size_bytes": out.SizeBytes})}); err != nil {
			return err
		}
		art, err = s.q.GetArtifactByIdentity(ctx, r.Identity)
		if err != nil {
			return err
		}
	}
	state := "invalid"
	if out.State == ResourceAvailable && out.VerifiedAt != nil {
		state = "valid"
	}
	size := int64(0)
	if out.SizeBytes != nil {
		size = *out.SizeBytes
	}
	verified := ""
	if out.VerifiedAt != nil {
		verified = out.VerifiedAt.UTC().Format(time.RFC3339Nano)
	}
	return s.q.UpsertPlacement(ctx, db.UpsertPlacementParams{ArtifactID: art.ID, NodeID: r.NodeID, Path: r.Destination, State: state, VerifiedAt: ns(verified), Diagnostics: "[]", SizeBytes: size})
}

func selectedArtifact(artifact recipe.Artifact, variant, root string) (ResourceSpec, error) {
	source, err := artifact.EffectiveSource(variant)
	if err != nil {
		return ResourceSpec{}, err
	}
	identity, err := artifactidentity.Canonical(source.Type, source.Identity, source.Revision, source.Digest)
	if err != nil {
		return ResourceSpec{}, err
	}
	spec := ResourceSpec{Kind: ResourceArtifact, Identity: identity, Source: SourceSpec{Type: SourceType(source.Type)}}
	switch spec.Source.Type {
	case SourceHuggingFace:
		spec.Source.Reference, spec.Source.Revision, _ = strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
		spec.Destination = path.Join(root, "hub", "models--"+strings.ReplaceAll(spec.Source.Reference, "/", "--"))
	case SourceFile, SourceLocal:
		spec.Source.Digest = strings.TrimPrefix(identity, "file://")
		if spec.Source.Type == SourceFile && strings.HasPrefix(source.Identity, "https://") {
			spec.Source.URL = source.Identity
		}
		spec.Destination = path.Join(root, "files", strings.TrimPrefix(spec.Source.Digest, "sha256:"))
	case SourceOCI:
		spec.Source.Reference, spec.Source.Digest, _ = strings.Cut(identity, "@")
		spec.Destination = path.Join(root, "oci", strings.TrimPrefix(spec.Source.Digest, "sha256:"))
	}
	if artifact.SizeBytes > 0 {
		size := artifact.SizeBytes
		spec.SizeBytes = &size
	}
	return spec, spec.Validate()
}

func (s *Service) SupportsNode(ctx context.Context, nodeID string) bool {
	node, err := s.q.GetNode(ctx, nodeID)
	return err == nil && hasFeature(nodeReport(node))
}
