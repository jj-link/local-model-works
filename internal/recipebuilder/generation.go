package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
	"github.com/jj-link/local-model-works/schemas"
)

// GenerateProposal runs an already-reserved generation operation against the
// exact draft and provider-settings versions the operator reviewed.
func (s *Service) GenerateProposal(ctx context.Context, draftID, operationID, runID string,
	approval GenerationApproval, provider recipeassistant.Provider,
	progress func(phase, message string)) (*Draft, error) {
	timeout := 10 * time.Minute
	if approval.Request.Mode == "add" {
		// Repository investigation spans multiple bounded provider turns.
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return nil, err
	}
	op, err := requireOperation(row, operationID, PhaseGenerate)
	if err != nil {
		return nil, err
	}
	if row.Version != approval.DraftVersion+1 || !validApproval(approval) {
		return s.failOperation(ctx, row, op, nil, nil, nil,
			newError("recipe.generation_consent_stale", "draft or provider settings changed after generation consent", false))
	}
	if provider == nil {
		return s.failOperation(ctx, row, op, nil, nil, nil,
			newError("recipe.provider_unavailable", "selected assistant provider is unavailable", true))
	}
	if err := s.attachRun(ctx, draftID, operationID, runID); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	request, err := s.assistantRequest(ctx, row, approval.Request.Instruction, approval.Request.Model, approvedDiagnosticIDs(approval.Request))
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	request.RunExcerpts = approval.Request.RunExcerpts
	check := approval
	check.Request = request
	digest, err := check.Digest()
	if err != nil || digest != approval.PreviewSHA256 {
		return s.failOperation(ctx, row, op, nil, nil, nil, newError("recipe.generation_consent_stale", "approved source content changed before generation", false))
	}
	if request.Mode == "add" {
		s.attachInvestigation(ctx, draftID, value(row.ResolvedCommit), &request)
	}
	result, err := provider.Generate(ctx, request, func(message string) {
		if progress != nil {
			progress(PhaseGenerate, message)
		}
	})
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	if request.Mode == "add" && len(result.Procedures) == 0 {
		return s.failOperation(ctx, row, op, nil, nil, nil, newError("recipe.proposal_invalid", "Repository investigation must return documented procedures with supporting evidence.", false))
	}
	pinEvidence := func(evidence []recipeassistant.Evidence) {
		for i := range evidence {
			if evidence[i].SourceCommit == "" && !strings.HasPrefix(evidence[i].SourcePath, "https://") {
				evidence[i].SourceCommit = value(row.ResolvedCommit)
			}
		}
	}
	pinEvidence(result.Evidence)
	for i := range result.Procedures {
		pinEvidence(result.Procedures[i].Evidence)
	}
	if err := validateProposalResult(result, request, renderCandidates(row.Candidates)); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	proposalID, err := id.New()
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	var proposal Proposal
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	if err := json.Unmarshal(resultJSON, &proposal); err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	proposal.ID, proposal.BaseVersion = proposalID, row.Version+1
	proposal.ProviderID, proposal.ProviderVersion = approval.ProviderID, approval.ProviderVersion
	proposal.Model, proposal.RunID, proposal.PreviewSHA256 = request.Model, runID, approval.PreviewSHA256
	if len(proposal.Procedures) != 0 {
		for i := range proposal.Procedures {
			if err := s.prepareProcedure(row, &proposal.Procedures[i]); err != nil {
				return s.failOperation(ctx, row, op, nil, nil, nil, err)
			}
		}
	} else {
		procedure := procedureFromProposal(proposal)
		if err := s.prepareProcedure(row, &procedure); err != nil {
			return s.failOperation(ctx, row, op, nil, nil, nil, err)
		}
		proposal.Manifest, proposal.Files, proposal.SelectedSourceAssets = procedure.Manifest, procedure.Files, procedure.SelectedSourceAssets
		proposal.Questions, proposal.Diagnostics, proposal.Adaptations = procedure.Questions, procedure.Diagnostics, procedure.Adaptations
	}
	normalizeProposalCollections(&proposal)
	encodedProposal, err := marshalJSON(proposal)
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	if s.db == nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, newError("recipe.draft_io", "draft database is unavailable", true))
	}
	resultSQL, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET state=?, proposal=?, run_id=?, operation=NULL,
version=version+1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND json_extract(operation,'$.id')=?`,
		op.PreviousState, encodedProposal, nullable(runID), draftID, row.Version, operationID)
	if err != nil {
		return s.failOperation(ctx, row, op, nil, nil, nil, err)
	}
	rows, err := resultSQL.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_operation_lost", "the active generation changed before proposal persistence", false)
	}
	return s.Get(ctx, draftID)
}

func (s *Service) assistantRequest(ctx context.Context, row db.RecipeDraft, instruction, model string, diagnosticIDs []string) (recipeassistant.Request, error) {
	var selected []ContextFile
	draft, err := render(row)
	if err != nil {
		return recipeassistant.Request{}, err
	}
	if err := json.Unmarshal([]byte(row.ContextSelection), &selected); err != nil {
		return recipeassistant.Request{}, newError("recipe.draft_context_invalid", "stored context selection is invalid", false)
	}
	request := recipeassistant.Request{Manifest: json.RawMessage(row.Manifest), Instruction: instruction, Model: model}
	request.RecipeSchema, err = schemas.FS.ReadFile("recipe/v1alpha1.schema.json")
	if err != nil {
		return recipeassistant.Request{}, err
	}
	request.SourcePins = map[string]any{"source": draft.Source, "commit": draft.ResolvedCommit, "tree": draft.ResolvedTree}
	if draft.ChangeContext != nil {
		request.SourcePins = map[string]any{"source": draft.Source, "commit": draft.ResolvedCommit, "tree": draft.ResolvedTree, "base_source": draft.ChangeContext.BaseSource, "base_commit": draft.ChangeContext.BaseCommit, "base_tree": draft.ChangeContext.BaseTree}
	}
	if draft.ChangeContext == nil || draft.ChangeContext.BaseRecipeDigest == "" {
		if err := s.investigationInventory(ctx, draft, &request); err != nil {
			return recipeassistant.Request{}, err
		}
		selected = nil
	} else {
		request.Mode = draft.ChangeContext.Kind
		request.SourceStatus = "Only explicitly selected pinned source context is available. Missing instructions require questions; saved helpers are not upstream evidence."
		if len(selected) == 0 {
			selected, request.SourceStatus = changeSourceContext(draft)
		}
	}
	request.ResultSchema = recipeassistant.ProposalOutputSchema(request.Mode)
	if row.Proposal.Valid {
		// Refinement sees the suggestion, not its provider metadata or unselected
		// diagnostics. It remains inert and part of the exact consent digest.
		var pending recipeassistant.Result
		if err := json.Unmarshal([]byte(row.Proposal.String), &pending); err != nil {
			return recipeassistant.Request{}, err
		}
		request.PendingProposal, err = json.Marshal(pending)
		if err != nil {
			return recipeassistant.Request{}, err
		}
	}
	for _, asset := range draft.SelectedAssets {
		if _, _, err := s.ReadOwnedFile(ctx, row.ID, asset.Path, asset.SHA256, ""); err != nil {
			return recipeassistant.Request{}, err
		}
		request.Assets = append(request.Assets, recipeassistant.Asset{Path: asset.Path, SHA256: asset.SHA256, Origin: asset.Origin})
	}
	perFile := map[string]int{}
	total := 0
	for _, item := range selected {
		if item.SourceCommit == "" {
			item.SourceCommit = draft.ResolvedCommit
		}
		inventory, err := contextInventory(draft, item.SourceCommit)
		if err != nil {
			return recipeassistant.Request{}, err
		}
		candidate, err := findCandidate(inventory, item.Path, item.SHA256, item.Origin)
		if err != nil {
			return recipeassistant.Request{}, err
		}
		item.Origin = candidate.Origin
		_, data, err := s.ReadOwnedFile(ctx, row.ID, item.Path, item.SHA256, item.SourceCommit)
		if err != nil {
			return recipeassistant.Request{}, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != item.SHA256 {
			return recipeassistant.Request{}, newError("recipe.generation_consent_stale", "selected source bytes no longer match their verified hash", false)
		}
		if (item.StartLine == 0) != (item.EndLine == 0) || item.StartLine < 0 || item.EndLine < item.StartLine ||
			(item.EndLine > contextLineCount(data)) {
			return recipeassistant.Request{}, newError("recipe.draft_context_invalid", "stored context range is invalid", false)
		}
		content := string(data)
		if item.StartLine > 0 {
			content = selectedLines(content, item.StartLine, item.EndLine)
		}
		perFile[contextIdentity(item)] += len(content)
		total += len(content)
		if perFile[contextIdentity(item)] > maxContextFile || total > maxContextTotal {
			return recipeassistant.Request{}, newError("recipe.draft_context_too_large", "stored context exceeds source budgets", false)
		}
		request.Context = append(request.Context, recipeassistant.ContextFile{
			Path: item.Path, SHA256: item.SHA256, Origin: item.Origin, SourceCommit: item.SourceCommit, StartLine: item.StartLine, EndLine: item.EndLine, Content: content,
		})
	}
	var questions []Question
	_ = json.Unmarshal([]byte(row.Questions), &questions)
	for _, question := range questions {
		request.Questions = append(request.Questions, recipeassistant.Question{ID: question.ID, Path: question.Path, Question: question.Question, Answer: question.Answer})
	}
	var diagnostics []Diagnostic
	_ = json.Unmarshal([]byte(row.Diagnostics), &diagnostics)
	wanted := make(map[string]bool, len(diagnosticIDs))
	for _, selectedID := range diagnosticIDs {
		if selectedID == "" || wanted[selectedID] {
			return recipeassistant.Request{}, newError("recipe.draft_diagnostic_invalid", "diagnostic selections must be unique IDs", false)
		}
		wanted[selectedID] = true
	}
	for _, diagnostic := range diagnostics {
		if !wanted[diagnostic.ID] {
			continue
		}
		delete(wanted, diagnostic.ID)
		request.Diagnostics = append(request.Diagnostics, recipeassistant.Diagnostic{ID: diagnostic.ID, Code: diagnostic.Code, Path: diagnostic.Path, Message: diagnostic.Message, Remediation: diagnostic.Remediation})
	}
	if len(wanted) != 0 {
		return recipeassistant.Request{}, newError("recipe.draft_diagnostic_invalid", "selected diagnostic does not belong to this draft", false)
	}
	return request, nil
}

func selectedLines(content string, start, end int) string {
	lines := strings.SplitAfter(content, "\n")
	if start < 1 || start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "")
}

func renderCandidates(encoded string) []Candidate {
	var candidates []Candidate
	_ = json.Unmarshal([]byte(encoded), &candidates)
	return candidates
}

func validateProposalResult(result recipeassistant.Result, request recipeassistant.Request, candidates []Candidate) error {
	if err := recipeassistant.ValidateResult(result); err != nil {
		return err
	}
	if len(result.Procedures) != 0 {
		if request.Mode != "add" {
			return newError("recipe.proposal_invalid", "An update or repair must retain one saved procedure.", false)
		}
		for _, procedure := range result.Procedures {
			if len(procedure.Evidence) == 0 {
				return newError("recipe.proposal_evidence_invalid", "Each procedure requires supporting source evidence: "+procedure.Name, false)
			}
			if err := validateProposalResult(recipeassistant.Result{Manifest: procedure.Manifest, Files: procedure.Files, SelectedSourceAssets: procedure.SelectedSourceAssets, Questions: procedure.Questions, Evidence: procedure.Evidence, Adaptations: procedure.Adaptations}, request, candidates); err != nil {
				return err
			}
		}
		return nil
	}
	selectedPaths := make(map[string]bool, len(result.Files)+len(result.SelectedSourceAssets))
	if err := validateSourceOwnedProposal(result, request); err != nil {
		return err
	}
	for _, file := range result.Files {
		selectedPaths[file.Path] = true
	}
	for _, selected := range result.SelectedSourceAssets {
		if selectedPaths[selected.Path] {
			return newError("recipe.proposal_asset_invalid", "proposal selects multiple contents for the same package path: "+selected.Path, false)
		}
		selectedPaths[selected.Path] = true
		if _, err := findCandidate(candidates, selected.Path, selected.SHA256, OriginSource); err != nil {
			return newError("recipe.proposal_asset_invalid", "proposal selected an unverified source asset: "+selected.Path, false)
		}
	}
	for _, evidence := range result.Evidence {
		valid := false
		for _, selected := range request.Context {
			if selected.Path != evidence.SourcePath || selected.SHA256 != evidence.SHA256 || selected.SourceCommit != evidence.SourceCommit {
				continue
			}
			start, end := selected.StartLine, selected.EndLine
			if start == 0 {
				start, end = 1, contextLineCount([]byte(selected.Content))
			}
			if evidence.StartLine >= start && evidence.EndLine >= evidence.StartLine && evidence.EndLine <= end {
				valid = true
				break
			}
		}
		if !valid {
			return newError("recipe.proposal_evidence_invalid", "proposal evidence is outside reviewed context: "+evidence.SourcePath, false)
		}
	}
	seenQuestions := map[string]bool{}
	for _, question := range result.Questions {
		if strings.TrimSpace(question.ID) == "" || strings.TrimSpace(question.Question) == "" || seenQuestions[question.ID] {
			return newError("recipe.proposal_question_invalid", "proposal questions require unique IDs and text", false)
		}
		seenQuestions[question.ID] = true
	}
	return nil
}
