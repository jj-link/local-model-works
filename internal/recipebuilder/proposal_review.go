package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/jj-link/local-model-works/internal/db"
)

// AcceptProposal materializes reviewed generated files into the draft's
// content-addressed store and applies the proposal under a version-and-ID CAS.
func (s *Service) AcceptProposal(ctx context.Context, draftID string, version int64, proposalID string) (*Draft, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	var prior ChangeContext
	if row.ChangeContext.Valid {
		if err := json.Unmarshal([]byte(row.ChangeContext.String), &prior); err != nil {
			return nil, err
		}
	}
	if prior.AcceptedProposalID == proposalID && prior.AcceptedVersion == version && row.Version == version+1 && !row.Operation.Valid {
		return s.Get(ctx, draftID)
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
	var proposal Proposal
	if !row.Proposal.Valid || json.Unmarshal([]byte(row.Proposal.String), &proposal) != nil || proposal.ID == "" || proposal.ID != proposalID || proposal.BaseVersion != row.Version {
		return nil, newError("recipe.proposal_stale", "proposal changed; refetch and review it again", false)
	}
	if proposal.ProviderID != "" && proposal.ProviderID != "compiler" && proposal.PreviewSHA256 == "" {
		return nil, newError("recipe.proposal_stale", "this suggestion predates explicit content approval; request a reviewed suggestion", false)
	}
	procedures := proposal.Procedures
	if len(procedures) == 0 {
		procedures = []Procedure{procedureFromProposal(proposal)}
	}
	if len(procedures) > 1 && prior.BaseRecipeDigest != "" {
		return nil, newError("recipe.proposal_invalid", "Updates and repairs must retain one saved procedure.", false)
	}
	if s.db == nil {
		return nil, newError("recipe.draft_io", "draft database is unavailable", true)
	}
	ids := make([]string, len(procedures))
	ids[0] = draftID
	for i := 1; i < len(ids); i++ {
		sum := sha256.Sum256([]byte(proposalID + "\x00" + procedures[i].ID))
		ids[i] = "procedure-" + hex.EncodeToString(sum[:16])
	}
	draft, err := render(row)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	for i := range procedures {
		procedure := &procedures[i]
		if err := s.prepareProcedure(row, procedure); err != nil {
			return nil, err
		}
		inventory := renderCandidates(row.Candidates)
		if err := s.verifyProcedureEvidence(ctx, draft, *procedure, inventory, proposal.ProviderID == "compiler"); err != nil {
			return nil, err
		}
		candidates, selected := proposalAssets(inventory, proposalFromProcedure(*procedure))
		if _, err := validateSelection(selected, candidates); err != nil {
			return nil, err
		}
		for _, asset := range procedure.SelectedSourceAssets {
			body, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+asset.SHA256))
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != asset.SHA256 {
				return nil, newError("recipe.draft_source_changed", "Selected upstream asset changed: "+asset.Path, false)
			}
		}
		target := filepath.Join(s.root, ids[i], "source")
		if err := os.MkdirAll(target, 0o700); err != nil {
			return nil, err
		}
		for _, file := range procedure.Files {
			sum := sha256.Sum256([]byte(file.Content))
			if err := storeGeneratedBlob(filepath.Join(target, "sha256-"+hex.EncodeToString(sum[:])), []byte(file.Content)); err != nil {
				return nil, err
			}
		}
		if i > 0 {
			// A sibling owns its own immutable blobs, including unselected source
			// needed for later questions and further repository investigation.
			hashes := make(map[string]bool, len(inventory)+len(procedure.Evidence))
			for _, candidate := range inventory {
				hashes[candidate.SHA256] = true
			}
			for _, evidence := range procedure.Evidence {
				hashes[evidence.SHA256] = true
			}
			for hash := range hashes {
				body, err := os.ReadFile(filepath.Join(s.root, draftID, "source", "sha256-"+hash))
				if err != nil {
					return nil, err
				}
				sum := sha256.Sum256(body)
				if hex.EncodeToString(sum[:]) != hash {
					return nil, newError("recipe.draft_source_changed", "source blob changed during acceptance", false)
				}
				if err := storeGeneratedBlob(filepath.Join(target, "sha256-"+hash), body); err != nil {
					return nil, err
				}
			}
			if exclusions, err := os.ReadFile(filepath.Join(s.root, draftID, "exclusions.json")); err == nil {
				if err := os.WriteFile(filepath.Join(s.root, ids[i], "exclusions.json"), exclusions, 0o600); err != nil {
					return nil, err
				}
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
		change := prior
		if change.Kind == "" {
			change.Kind = "add"
		}
		change.Review = procedure
		change.RelatedDraftIDs = nil
		for _, other := range ids {
			if other != ids[i] {
				change.RelatedDraftIDs = append(change.RelatedDraftIDs, other)
			}
		}
		change.AcceptedProposalID, change.AcceptedVersion = proposalID, version
		changeJSON, err := marshalJSON(change)
		if err != nil {
			return nil, err
		}
		contextJSON := row.ContextSelection
		if upstreamManifest(procedure.Manifest) {
			var contextFiles []ContextFile
			if err := json.Unmarshal([]byte(contextJSON), &contextFiles); err != nil {
				return nil, err
			}
			kept := contextFiles[:0]
			for _, item := range contextFiles {
				sourceInventory := inventory
				if item.SourceCommit != "" && item.SourceCommit != value(row.ResolvedCommit) && item.SourceCommit == prior.BaseCommit {
					sourceInventory = prior.BaseCandidates
				}
				candidate, err := findCandidate(sourceInventory, item.Path, item.SHA256, item.Origin)
				if err == nil && candidate.Origin == OriginSource {
					kept = append(kept, item)
				}
			}
			contextJSON, err = marshalJSON(nonNilSlice(kept))
			if err != nil {
				return nil, err
			}
		}
		state := editableState(procedure.Manifest, procedure.Questions, candidates, selected, procedure.Diagnostics, nil)
		candidatesJSON, _ := marshalJSON(candidates)
		selectedJSON, _ := marshalJSON(selected)
		questionsJSON, _ := marshalJSON(procedure.Questions)
		findingsJSON, _ := marshalJSON(procedure.Diagnostics)
		if i == 0 {
			result, err := tx.ExecContext(ctx, `UPDATE recipe_drafts SET state=?, manifest=?, candidates=?, selected_assets=?, diagnostics=?,
proposal=NULL, questions=?, resolved_references='[]', acknowledged_warnings='[]', package_digest=NULL, change_context=?, context_selection=?, version=version+1,
updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND json_extract(proposal,'$.id')=? AND json_extract(proposal,'$.base_version')=version AND operation IS NULL AND state!='installed'`,
				state, string(procedure.Manifest), candidatesJSON, selectedJSON, findingsJSON, questionsJSON, changeJSON, contextJSON, draftID, version, proposalID)
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
		} else {
			if err := qtx.CreateRecipeDraft(ctx, db.CreateRecipeDraftParams{
				ID: ids[i], State: state, Source: row.Source, ResolvedCommit: row.ResolvedCommit, ResolvedTree: row.ResolvedTree,
				Manifest: string(procedure.Manifest), Candidates: candidatesJSON, SelectedAssets: selectedJSON, Diagnostics: findingsJSON,
				ContextSelection: contextJSON, Questions: questionsJSON, AcknowledgedWarnings: "[]", ResolvedReferences: "[]",
				ParentDraftID: nullable(draftID), ChangeContext: nullable(changeJSON),
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
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
