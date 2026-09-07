package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
)

// AcceptProposal materializes reviewed generated files into the draft's
// content-addressed store and applies the proposal under a version-and-ID CAS.
func (s *Service) AcceptProposal(ctx context.Context, draftID string, version int64, proposalID string) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Version != version {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	if row.State == "installed" {
		return nil, newError("recipe.draft_immutable", "installed drafts are immutable", false)
	}
	if !row.Proposal.Valid {
		return nil, newError("recipe.proposal_unknown", "draft has no proposal to accept", false)
	}
	var proposal Proposal
	if err := json.Unmarshal([]byte(row.Proposal.String), &proposal); err != nil || proposal.ID == "" || proposal.ID != proposalID {
		return nil, newError("recipe.proposal_stale", "proposal changed; refetch and review it again", false)
	}
	if proposal.BaseVersion != row.Version {
		return nil, newError("recipe.proposal_stale", "draft changed since this proposal was generated; review a new suggestion", false)
	}
	if proposal.ProviderID != "" && proposal.ProviderID != "compiler" && proposal.PreviewSHA256 == "" {
		return nil, newError("recipe.proposal_stale", "this suggestion predates explicit content approval; discard it and request a reviewed suggestion", false)
	}
	proposal.Manifest, err = pinnedManifest(row, proposal.Manifest)
	if err != nil {
		return nil, err
	}
	proposal.Questions = proposalQuestions(renderQuestions(row.Questions), proposal.Questions)
	candidates, selected := proposalAssets(renderCandidates(row.Candidates), proposal)
	for _, file := range proposal.Files {
		sum := sha256.Sum256([]byte(file.Content))
		hash := hex.EncodeToString(sum[:])
		blob := filepath.Join(s.root, draftID, "source", "sha256-"+hash)
		if err := storeGeneratedBlob(blob, []byte(file.Content)); err != nil {
			return nil, err
		}
	}
	findings := append(retainedFindings(row.Diagnostics), s.validatorFindings(proposal.Manifest)...)
	findings = append(findings, manifestAssetFindings(proposal.Manifest, candidates, selected)...)
	for i := range findings {
		if findings[i].ID == "" {
			findings[i].ID = DiagnosticID(findings[i], proposal.ID)
		}
	}
	state := editableState(proposal.Manifest, proposal.Questions, candidates, selected, findings, nil)
	manifestJSON := string(proposal.Manifest)
	candidatesJSON, _ := marshalJSON(candidates)
	selectedJSON, _ := marshalJSON(selected)
	questionsJSON, _ := marshalJSON(proposal.Questions)
	findingsJSON, _ := marshalJSON(findings)
	result, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET state=?, manifest=?, candidates=?, selected_assets=?, diagnostics=?,
proposal=NULL, questions=?, resolved_references='[]', acknowledged_warnings='[]', package_digest=NULL, version=version+1,
updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND json_extract(proposal,'$.id')=? AND json_extract(proposal,'$.base_version')=version AND operation IS NULL AND state!='installed'`,
		state, manifestJSON, candidatesJSON, selectedJSON, findingsJSON, questionsJSON, draftID, version, proposalID)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.proposal_stale", "proposal changed; refetch and review it again", false)
	}
	return s.Get(ctx, draftID)
}

// DiscardProposal removes only the exact reviewed proposal; draft content is unchanged.
func (s *Service) DiscardProposal(ctx context.Context, draftID string, version int64, proposalID string) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	if row.Version != version {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	if row.Operation.Valid {
		return nil, newError("recipe.draft_operation_active", "an operation already owns this draft", false)
	}
	if row.State == "installed" {
		return nil, newError("recipe.draft_immutable", "installed drafts are immutable", false)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET proposal=NULL, version=version+1,
updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND json_extract(proposal,'$.id')=?`, draftID, version, proposalID)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.proposal_stale", "proposal changed; refetch and review it again", false)
	}
	return s.Get(ctx, draftID)
}

// Only the operator can answer questions. Preserve existing questions omitted
// by a model and preserve answers only when the question itself is unchanged.
func proposalQuestions(existing, suggested []Question) []Question {
	out := append([]Question(nil), existing...)
	for _, question := range suggested {
		question.Answer = ""
		found := false
		for i := range out {
			if out[i].ID != question.ID {
				continue
			}
			if out[i].Path == question.Path && out[i].Question == question.Question {
				question.Answer = out[i].Answer
			}
			out[i] = question
			found = true
			break
		}
		if !found {
			out = append(out, question)
		}
	}
	return out
}
