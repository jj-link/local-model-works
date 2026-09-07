package recipebuilder

import (
	"context"
)

// DismissDiagnostics removes only explicit terminal operation-attempt
// diagnostics. Validator, source-safety, and warning findings remain owned by
// their causes and cannot be hidden.
func (s *Service) DismissDiagnostics(ctx context.Context, draftID string, version int64, diagnosticIDs []string) (*Draft, error) {
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
	draft, err := render(row)
	if err != nil {
		return nil, err
	}
	requested := make(map[string]bool, len(diagnosticIDs))
	for _, diagnosticID := range diagnosticIDs {
		if diagnosticID == "" || requested[diagnosticID] {
			return nil, newError("recipe.diagnostic_unknown", "diagnostic IDs must be non-empty and unique", false)
		}
		requested[diagnosticID] = true
	}
	for diagnosticID := range requested {
		found := false
		for _, diagnostic := range draft.Diagnostics {
			if diagnostic.ID != diagnosticID {
				continue
			}
			found = true
			if !diagnostic.Dismissible || diagnostic.Phase == PhaseValidate || diagnostic.Severity == "warning" {
				return nil, newError("recipe.diagnostic_not_dismissible", "the selected diagnostic must be fixed or acknowledged", false)
			}
			break
		}
		if !found {
			return nil, newError("recipe.diagnostic_unknown", "selected diagnostic is no longer current", false)
		}
	}
	kept := draft.Diagnostics[:0]
	for _, diagnostic := range draft.Diagnostics {
		if !requested[diagnostic.ID] {
			kept = append(kept, diagnostic)
		}
	}
	encoded, err := marshalJSON(kept)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recipe_drafts SET diagnostics=?, version=version+1,
updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND version=? AND operation IS NULL`, encoded, draftID, version)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows != 1 {
		return nil, newError("recipe.draft_stale_version", "draft version changed; refetch and retry", false)
	}
	return s.Get(ctx, draftID)
}
