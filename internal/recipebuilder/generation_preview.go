package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jj-link/local-model-works/internal/cjson"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

// GenerationApproval is the immutable, credential-free content approved by the operator.
type GenerationApproval struct {
	DraftVersion    int64                   `json:"draft_version"`
	ProviderID      string                  `json:"provider_id"`
	ProviderVersion string                  `json:"provider_version"`
	Destination     string                  `json:"destination"`
	ProviderKind    string                  `json:"provider_kind"`
	CredentialID    string                  `json:"credential_id,omitempty"`
	Request         recipeassistant.Request `json:"request"`
	PreviewSHA256   string                  `json:"preview_sha256"`
}

func (approval GenerationApproval) Digest() (string, error) {
	approval.PreviewSHA256 = ""
	canonical, err := cjson.Marshal(approval)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// GenerationRequest reads and rehashes reviewed source bytes, without contacting a provider.
func (s *Service) GenerationRequest(ctx context.Context, draftID string, version int64, instruction, model string, diagnosticIDs []string) (recipeassistant.Request, error) {
	row, err := s.getDraftRow(ctx, draftID)
	if err != nil {
		return recipeassistant.Request{}, err
	}
	if row.Version != version {
		return recipeassistant.Request{}, newError("recipe.generation_consent_stale", "draft changed after review", false)
	}
	if row.State == "installed" {
		return recipeassistant.Request{}, newError("recipe.draft_immutable", "installed drafts cannot be changed", false)
	}
	if row.Operation.Valid {
		return recipeassistant.Request{}, newError("recipe.draft_operation_active", "draft has an active operation", false)
	}
	return s.assistantRequest(ctx, row, instruction, model, diagnosticIDs)
}

func approvedDiagnosticIDs(request recipeassistant.Request) []string {
	ids := make([]string, 0, len(request.Diagnostics))
	for _, diagnostic := range request.Diagnostics {
		ids = append(ids, diagnostic.ID)
	}
	return ids
}

func validApproval(approval GenerationApproval) bool {
	digest, err := approval.Digest()
	return err == nil && digest == approval.PreviewSHA256 && strings.TrimSpace(approval.ProviderID) != "" && strings.TrimSpace(approval.ProviderVersion) != "" && approval.Destination != "" && approval.Request.Model != ""
}
