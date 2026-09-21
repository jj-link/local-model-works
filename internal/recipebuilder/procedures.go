package recipebuilder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

// Public array fields stay arrays even when a provider or stored row uses null.
func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func normalizeProcedureCollections(procedure *Procedure) {
	procedure.Files = nonNilSlice(procedure.Files)
	procedure.SelectedSourceAssets = nonNilSlice(procedure.SelectedSourceAssets)
	procedure.Questions = nonNilSlice(procedure.Questions)
	procedure.Evidence = nonNilSlice(procedure.Evidence)
}

func normalizeProposalCollections(proposal *Proposal) {
	if len(proposal.Procedures) > 0 {
		proposal.Manifest = json.RawMessage(`{}`)
	}
	proposal.Files = nonNilSlice(proposal.Files)
	proposal.SelectedSourceAssets = nonNilSlice(proposal.SelectedSourceAssets)
	proposal.Questions = nonNilSlice(proposal.Questions)
	proposal.Evidence = nonNilSlice(proposal.Evidence)
	proposal.Diagnostics = nonNilSlice(proposal.Diagnostics)
	for index := range proposal.Procedures {
		normalizeProcedureCollections(&proposal.Procedures[index])
	}
}

func procedureFromProposal(proposal Proposal) Procedure {
	return Procedure{Manifest: proposal.Manifest, Files: proposal.Files, SelectedSourceAssets: proposal.SelectedSourceAssets, Questions: proposal.Questions, Evidence: proposal.Evidence, Summary: proposal.Summary, Adaptations: proposal.Adaptations, Diagnostics: proposal.Diagnostics}
}

func proposalFromProcedure(procedure Procedure) Proposal {
	return Proposal{Manifest: procedure.Manifest, Files: procedure.Files, SelectedSourceAssets: procedure.SelectedSourceAssets, Questions: procedure.Questions, Evidence: procedure.Evidence, Summary: procedure.Summary, Adaptations: procedure.Adaptations, Diagnostics: procedure.Diagnostics}
}

func (s *Service) prepareProcedure(row db.RecipeDraft, procedure *Procedure) error {
	// Recheck at acceptance, including compiler and persisted suggestions, before
	// generated content can be materialized or selected into the draft package.
	encoded, err := json.Marshal(procedure)
	if err != nil {
		return err
	}
	var result recipeassistant.Result
	if err := json.Unmarshal(encoded, &result); err != nil {
		return err
	}
	if err := recipeassistant.ValidateResult(result); err != nil {
		return err
	}
	if upstreamManifest([]byte(row.Manifest)) && !upstreamManifest(procedure.Manifest) {
		return newError("recipe.proposal_upstream_required", "Source-owned execution cannot be replaced by a generated serving stack.", false)
	}
	var change ChangeContext
	if row.ChangeContext.Valid {
		if err := json.Unmarshal([]byte(row.ChangeContext.String), &change); err != nil {
			return err
		}
	}
	if change.BaseRecipeDigest != "" {
		if parsed, err := recipe.Parse(change.BaseManifest); err == nil && parsed.Metadata.Source != nil {
			procedure.ID = parsed.Metadata.Source.Procedure
		}
	} else if procedure.ID != "" {
		change.Review = procedure
		encoded, err := marshalJSON(change)
		if err != nil {
			return err
		}
		row.ChangeContext = nullable(encoded)
	}
	procedure.Manifest, err = pinnedManifest(row, procedure.Manifest)
	if err != nil {
		return err
	}
	if parsed, err := recipe.Parse(procedure.Manifest); err == nil {
		if procedure.Name == "" {
			procedure.Name = parsed.Metadata.Name
		}
		if procedure.Description == "" {
			procedure.Description = parsed.Metadata.Description
		}
	}
	procedure.Questions = proposalQuestions(renderQuestions(row.Questions), procedure.Questions)
	inventory := renderCandidates(row.Candidates)
	files := make([]ProposalFile, 0, len(procedure.Files))
	for _, file := range procedure.Files {
		sum := sha256.Sum256([]byte(file.Content))
		hash := hex.EncodeToString(sum[:])
		if _, err := findCandidate(inventory, file.Path, hash, OriginSource); err == nil {
			procedure.SelectedSourceAssets = append(procedure.SelectedSourceAssets, AssetSelection{Path: file.Path, SHA256: hash, Origin: OriginSource})
		} else {
			files = append(files, file)
		}
	}
	procedure.Files = files
	for i := range procedure.SelectedSourceAssets {
		procedure.SelectedSourceAssets[i].Origin = OriginSource
	}
	proposal := proposalFromProcedure(*procedure)
	candidates, selected := proposalAssets(inventory, proposal)
	procedure.Diagnostics = append(retainedFindings(row.Diagnostics), s.validatorFindings(procedure.Manifest)...)
	procedure.Diagnostics = append(procedure.Diagnostics, manifestAssetFindings(procedure.Manifest, candidates, selected)...)
	// A successfully reviewed source contract supersedes a failed compiler
	// attempt; ordinary draft edits cannot dismiss this blocking boundary.
	if upstreamManifest(procedure.Manifest) || len(procedure.Files) == 0 {
		kept := procedure.Diagnostics[:0]
		for _, finding := range procedure.Diagnostics {
			if finding.Code != "recipe.compile_failed" {
				kept = append(kept, finding)
			}
		}
		procedure.Diagnostics = kept
	}
	return nil
}
