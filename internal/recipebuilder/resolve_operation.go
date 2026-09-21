package recipebuilder

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// ResolveReferences verifies mutable external references for one reserved
// operation and commits content plus evidence only after every lookup succeeds.
func (s *Service) ResolveReferences(ctx context.Context, draftID, operationID, runID string, input ResolveRequest,
	resolver *ReferenceResolver, progress func(phase, message string)) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, PhaseResolve)
	if err != nil {
		return nil, err
	}
	if resolver == nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, newError("recipe.reference_unavailable", "reference resolver is unavailable", true))
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	var manifest recipe.Manifest
	if err := json.Unmarshal([]byte(row.Manifest), &manifest); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, newError("recipe.reference_manifest_invalid", "manifest must be valid JSON before resolving references", false))
	}
	report := func(message string) {
		if progress != nil {
			progress(PhaseResolve, message)
		}
	}
	report("verifying container images")
	evidence := make([]ResolvedReference, 0)
	resolveImage := func(pointer string, image *recipe.Image) error {
		normalized, err := normalizeImageReference(image.Reference)
		if err != nil {
			return err
		}
		image.Reference = normalized
		ref, parseErr := registry.ParseReference(image.Reference)
		if parseErr != nil {
			return newError("recipe.reference_invalid", "container reference is invalid at "+pointer, false)
		}
		credential, credentialErr := referenceCredential(pointer, input.Credentials, ref.Registry)
		if credentialErr != nil {
			return credentialErr
		}
		digest, resolveErr := resolver.ResolveImage(ctx, image.Reference, ref.Registry, credential)
		if resolveErr != nil {
			return resolveErr
		}
		if image.Digest != "" && !strings.EqualFold(image.Digest, digest) {
			return newError("recipe.reference_digest_mismatch", "registry digest differs from the supplied immutable digest at "+pointer, false)
		}
		image.Digest = digest
		image.Reference = ref.Registry + "/" + ref.Repository + "@" + digest
		evidence = append(evidence, ResolvedReference{Path: pointer, InputIdentity: image.Reference + "|credential:" + credential, ResolvedValue: digest, Origin: "registry", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)})
		return nil
	}
	for i := range manifest.Workloads {
		if manifest.Workloads[i].Upstream != nil {
			continue
		}
		if err := resolveImage("/workloads/"+itoa(i)+"/image", &manifest.Workloads[i].Image); err != nil {
			return s.failOperation(ctx, row, op, nil, nil, nil, err)
		}
	}
	if manifest.Prepare != nil {
		if err := resolveImage("/prepare/image", &manifest.Prepare.Image); err != nil {
			return s.failOperation(ctx, row, op, nil, nil, nil, err)
		}
	}
	if manifest.Verify != nil {
		if err := resolveImage("/verify/image", &manifest.Verify.Image); err != nil {
			return s.failOperation(ctx, row, op, nil, nil, nil, err)
		}
	}

	report("verifying artifact references")
	checksums := make(map[string]FileChecksum, len(input.FileChecksums))
	for _, checksum := range input.FileChecksums {
		checksums[checksum.Path] = checksum
	}
	resolveSource := func(pointer string, source *recipe.ArtSource) error {
		switch source.Type {
		case "huggingface":
			credential, credentialErr := referenceCredential(pointer, input.Credentials, "huggingface.co")
			if credentialErr != nil {
				return credentialErr
			}
			inputRevision := source.Revision
			commit, resolveErr := resolver.ResolveHuggingFace(ctx, source.Identity, inputRevision, credential)
			if resolveErr != nil {
				return resolveErr
			}
			source.Revision = commit
			evidence = append(evidence, ResolvedReference{Path: pointer, InputIdentity: source.Identity + "@" + commit + "|credential:" + credential, ResolvedValue: commit, Origin: "huggingface", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)})
		case "oci":
			ref, parseErr := registry.ParseReference(source.Identity)
			if parseErr != nil {
				return newError("recipe.reference_invalid", "OCI artifact reference is invalid at "+pointer, false)
			}
			credential, credentialErr := referenceCredential(pointer, input.Credentials, ref.Registry)
			if credentialErr != nil {
				return credentialErr
			}
			digest, resolveErr := resolver.ResolveImage(ctx, source.Identity, ref.Registry, credential)
			if resolveErr != nil {
				return resolveErr
			}
			if source.Digest != "" && !strings.EqualFold(source.Digest, digest) {
				return newError("recipe.reference_digest_mismatch", "registry digest differs at "+pointer, false)
			}
			source.Digest = digest
			evidence = append(evidence, ResolvedReference{Path: pointer, InputIdentity: source.Identity + "|credential:" + credential, ResolvedValue: digest, Origin: "registry", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)})
		case "file":
			checksum, ok := checksums[pointer]
			if !ok {
				return newError("recipe.reference_checksum_required", "operator checksum evidence is required at "+pointer, false)
			}
			verified, verifyErr := checksumEvidence(checksum)
			if verifyErr != nil {
				return verifyErr
			}
			if checksum.Path != pointer || checksum.URL != source.Identity {
				return newError("recipe.reference_operator_invalid", "operator checksum does not match the current file reference", false)
			}
			if source.Digest != "" && !strings.EqualFold(source.Digest, verified.ResolvedValue) {
				return newError("recipe.reference_digest_mismatch", "operator checksum differs at "+pointer, false)
			}
			source.Digest = verified.ResolvedValue
			evidence = append(evidence, verified)
		}
		return nil
	}
	for i := range manifest.Artifacts {
		artifact := &manifest.Artifacts[i]
		if artifact.Source != nil {
			if err := resolveSource("/artifacts/"+itoa(i)+"/source", artifact.Source); err != nil {
				return s.failOperation(ctx, row, op, nil, nil, nil, err)
			}
		}
		for j := range artifact.Variants {
			if err := resolveSource("/artifacts/"+itoa(i)+"/variants/"+itoa(j)+"/source", &artifact.Variants[j].Source); err != nil {
				return s.failOperation(ctx, row, op, nil, nil, nil, err)
			}
		}
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	findings := append(retainedFindings(row.Diagnostics), s.validatorFindings(manifestJSON)...)
	var candidates []Candidate
	_ = json.Unmarshal([]byte(row.Candidates), &candidates)
	var selected []AssetSelection
	_ = json.Unmarshal([]byte(row.SelectedAssets), &selected)
	findings = append(findings, manifestAssetFindings(manifestJSON, candidates, selected)...)
	state := editableState(manifestJSON, renderQuestions(row.Questions), candidates, selected, findings, evidence)
	findingsJSON, _ := marshalJSON(findings)
	referencesJSON, _ := marshalJSON(evidence)
	rows, err := s.q.CompleteRecipeDraftOperation(ctx, db.CompleteRecipeDraftOperationParams{
		State: state, Source: row.Source, ResolvedCommit: row.ResolvedCommit, ResolvedTree: row.ResolvedTree,
		Manifest: string(manifestJSON), Candidates: row.Candidates, SelectedAssets: row.SelectedAssets,
		Diagnostics: findingsJSON, PackageDigest: nullable(""), RunID: nullable(runID), Proposal: row.Proposal,
		ContextSelection: row.ContextSelection, Questions: row.Questions, AcknowledgedWarnings: "[]",
		ResolvedReferences: referencesJSON, ParentDraftID: row.ParentDraftID, ChangeContext: row.ChangeContext, ID: draftID, Operation: nullable(operationID),
	})
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	if rows != 1 {
		return nil, newError("recipe.draft_operation_lost", "the reference operation changed before completion", false)
	}
	return s.Get(ctx, draftID)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}

func referenceEvidenceReady(manifest *recipe.Manifest, evidence []ResolvedReference) bool {
	byPath := make(map[string]ResolvedReference, len(evidence))
	for _, item := range evidence {
		byPath[item.Path] = item
	}
	imageReady := func(pointer string, image recipe.Image) bool {
		item, ok := byPath[pointer]
		return ok && item.Origin == "registry" && item.ResolvedValue == image.Digest &&
			strings.HasPrefix(item.InputIdentity, image.Reference+"|credential:")
	}
	for i, workload := range manifest.Workloads {
		if workload.Upstream != nil {
			continue
		}
		if !imageReady("/workloads/"+itoa(i)+"/image", workload.Image) {
			return false
		}
	}
	if manifest.Prepare != nil && !imageReady("/prepare/image", manifest.Prepare.Image) {
		return false
	}
	if manifest.Verify != nil && !imageReady("/verify/image", manifest.Verify.Image) {
		return false
	}
	sourceReady := func(pointer string, source recipe.ArtSource) bool {
		item, ok := byPath[pointer]
		if !ok {
			return false
		}
		switch source.Type {
		case "huggingface":
			return item.Origin == "huggingface" && item.ResolvedValue == source.Revision &&
				strings.HasPrefix(item.InputIdentity, source.Identity+"@"+source.Revision+"|credential:")
		case "oci":
			return item.Origin == "registry" && item.ResolvedValue == source.Digest &&
				strings.HasPrefix(item.InputIdentity, source.Identity+"|credential:")
		case "file":
			return item.Origin == "operator" && item.ResolvedValue == source.Digest &&
				strings.HasPrefix(item.InputIdentity, source.Identity+"#sha256=")
		default:
			return false
		}
	}
	for i, artifact := range manifest.Artifacts {
		if artifact.Source != nil && !sourceReady("/artifacts/"+itoa(i)+"/source", *artifact.Source) {
			return false
		}
		for j, variant := range artifact.Variants {
			if !sourceReady("/artifacts/"+itoa(i)+"/variants/"+itoa(j)+"/source", variant.Source) {
				return false
			}
		}
	}
	return true
}
