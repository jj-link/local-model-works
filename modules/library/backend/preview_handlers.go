package backend

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

type runExcerptSelection struct {
	RunID        string `json:"run_id"`
	DeploymentID string `json:"deployment_id"`
	Rank         int32  `json:"rank"`
	Stream       string `json:"stream"`
	Offset       uint64 `json:"offset"`
	ByteCount    int    `json:"byte_count"`
}

type generationSelection struct {
	ProviderID      string                `json:"provider_id"`
	ProviderVersion string                `json:"provider_version"`
	Model           string                `json:"model"`
	Instruction     string                `json:"instruction"`
	DiagnosticIDs   []string              `json:"diagnostic_ids"`
	RunExcerpts     []runExcerptSelection `json:"run_excerpts"`
	PreviewSHA256   string                `json:"preview_sha256"`
	ContentConsent  bool                  `json:"content_consent"`
}

type generationExecution struct {
	DraftID     string                           `json:"draft_id"`
	OperationID string                           `json:"operation_id"`
	Approval    recipebuilder.GenerationApproval `json:"approval"`
	Provider    assistantProviderConfig          `json:"provider"`
}

func consentStale(message string) error {
	return &recipebuilder.Error{Code: "recipe.generation_consent_stale", Message: message}
}
func invalidSelection(message string) error {
	return &recipebuilder.Error{Code: "recipe.draft_generation_selection_invalid", Message: message}
}

func (m *Module) previewRecipeGeneration(w http.ResponseWriter, r *http.Request) {
	version, ok := requireDraftVersion(w, r)
	if !ok {
		return
	}
	var input generationSelection
	if err := httpx.DecodeBody(r, &input); err != nil {
		httpx.WriteErr(w, 422, "recipe.draft_invalid", err.Error())
		return
	}
	approval, _, err := m.prepareGeneration(r.Context(), chi.URLParam(r, "id"), version, input)
	if err != nil {
		writeDraftError(w, err, chi.URLParam(r, "id"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	setDraftETag(w, version)
	httpx.WriteJSON(w, 200, map[string]any{"provider_id": approval.ProviderID, "provider_version": approval.ProviderVersion, "destination": approval.Destination, "model": approval.Request.Model, "request": approval.Request, "preview_sha256": approval.PreviewSHA256, "warnings": []string{"Automatic redaction may miss secrets. Review all selected content before sending."}})
}

func (m *Module) resolveEnrolledLocal(ctx context.Context, deploymentID string) (recipeassistant.LocalEndpoint, error) {
	endpoint, err := recipeassistant.ResolveLocalEndpoint(ctx, m.env.Deploy, deploymentID)
	if err != nil {
		return endpoint, err
	}
	deployment, err := m.env.Deploy.Get(ctx, deploymentID)
	if err != nil {
		return recipeassistant.LocalEndpoint{}, err
	}
	if len(deployment.Placements) == 0 {
		return recipeassistant.LocalEndpoint{}, &recipeassistant.Error{Code: "assistant.deployment_not_ready", Message: "local provider has no enrolled placement", Retryable: true}
	}
	for _, placement := range deployment.Placements {
		node, err := m.env.Q.GetNode(ctx, placement.NodeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return recipeassistant.LocalEndpoint{}, err
		}
		if errors.Is(err, sql.ErrNoRows) || node.Status != "online" {
			return recipeassistant.LocalEndpoint{}, &recipeassistant.Error{Code: "assistant.deployment_not_ready", Message: "local provider requires online enrolled devices", Retryable: true}
		}
	}
	return endpoint, nil
}

func (m *Module) prepareGeneration(ctx context.Context, draftID string, version int64, input generationSelection) (recipebuilder.GenerationApproval, assistantProviderConfig, error) {
	var approval recipebuilder.GenerationApproval
	var config assistantProviderConfig
	if input.DiagnosticIDs == nil || input.RunExcerpts == nil {
		return approval, config, invalidSelection("explicit diagnostic_ids and run_excerpts selections are required (empty arrays select none)")
	}
	if len(input.Instruction) > 32<<10 {
		return approval, config, invalidSelection("instruction exceeds 32 KiB")
	}
	stored, settingsVersion, err := m.env.Settings.Get(ctx, "library")
	if err != nil {
		return approval, config, err
	}
	if input.ProviderVersion != settingsVersion {
		return approval, config, consentStale("provider settings changed after review")
	}
	providers, err := m.assistantProviders(ctx, stored)
	if err != nil {
		return approval, config, err
	}
	config, err = selectAssistantProvider(providers, input.ProviderID)
	if err != nil {
		return approval, config, err
	}
	if (config.Kind == "local" || config.Kind == "codex") && input.Model != "" && input.Model != config.Model {
		return approval, config, invalidSelection("provider model must match the selected deployment or account model")
	}
	if input.Model != "" {
		config.Model = input.Model
	}
	switch config.Kind {
	case "local":
		endpoint, err := m.resolveEnrolledLocal(ctx, config.DeploymentID)
		if err != nil {
			return approval, config, err
		}
		if input.Model != "" && input.Model != endpoint.Model {
			return approval, config, invalidSelection("local provider model must match its selected deployment")
		}
		config.BaseURL, config.Model, config.APIKeySecretID = endpoint.URL, endpoint.Model, ""
	case "openai_compatible":
		if err := recipeassistant.ValidateEndpoint(config.BaseURL); err != nil {
			return approval, config, err
		}
		if config.APIKeySecretID != "" {
			if _, err := recipeassistant.EncryptedSecretResolver(m.env.Q, m.env.Secrets)(ctx, config.APIKeySecretID); err != nil {
				return approval, config, err
			}
		}
	case "codex":
		config.BaseURL, config.APIKeySecretID = "codex://saved-account", ""
	default:
		return approval, config, invalidSelection("provider kind is invalid")
	}
	if strings.TrimSpace(config.Model) == "" {
		return approval, config, invalidSelection("select an explicit provider model")
	}
	outbound, err := m.env.RecipeBuilder.GenerationRequest(ctx, draftID, version, input.Instruction, config.Model, input.DiagnosticIDs)
	if err != nil {
		return approval, config, err
	}
	draft, err := m.env.RecipeBuilder.Get(ctx, draftID)
	if err != nil {
		return approval, config, err
	}
	outbound.RunExcerpts, err = m.selectedGenerationLogs(ctx, draft, input.RunExcerpts)
	if err != nil {
		return approval, config, err
	}
	approval = recipebuilder.GenerationApproval{DraftVersion: version, ProviderID: config.ID, ProviderVersion: settingsVersion, ProviderKind: config.Kind, Destination: config.BaseURL, CredentialID: config.APIKeySecretID, Request: outbound}
	approval.PreviewSHA256, err = approval.Digest()
	return approval, config, err
}

func (m *Module) executeGeneration(ctx context.Context, job *jobs.Context) (output map[string]any, resultErr error) {
	var input generationExecution
	raw, err := json.Marshal(job.Input)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = m.env.RecipeBuilder.FailOperation(cleanup, input.DraftID, input.OperationID, resultErr)
		}
	}()
	approval, config := input.Approval, input.Provider
	hash, err := approval.Digest()
	if err != nil || hash != approval.PreviewSHA256 || config.ID != approval.ProviderID || config.Kind != approval.ProviderKind || config.BaseURL != approval.Destination || config.Model != approval.Request.Model || config.APIKeySecretID != approval.CredentialID {
		return nil, consentStale("generation input does not match approved content and destination")
	}
	var provider recipeassistant.Provider
	switch config.Kind {
	case "local":
		endpoint, err := m.resolveEnrolledLocal(ctx, config.DeploymentID)
		if err != nil {
			return nil, err
		}
		if endpoint.URL != config.BaseURL || endpoint.Model != config.Model {
			return nil, consentStale("local provider endpoint changed after review")
		}
		provider, err = recipeassistant.LocalProvider(endpoint)
		if err != nil {
			return nil, err
		}
	case "openai_compatible":
		provider = &recipeassistant.OpenAICompatible{BaseURL: config.BaseURL, Model: config.Model, SecretID: config.APIKeySecretID, ResolveSecret: recipeassistant.EncryptedSecretResolver(m.env.Q, m.env.Secrets)}
	case "codex":
		if m.env.RecipeAssistant == nil {
			return nil, &recipeassistant.Error{Code: "assistant.codex_unavailable", Message: "Codex provider is unavailable", Retryable: true}
		}
		provider = m.env.RecipeAssistant
	default:
		return nil, invalidSelection("provider kind is invalid")
	}
	draft, err := m.env.RecipeBuilder.GenerateProposal(ctx, input.DraftID, input.OperationID, job.RunID, approval, provider, m.draftProgress(ctx, job, draftOperationInput{DraftID: input.DraftID, OperationID: input.OperationID}))
	if err != nil {
		return nil, err
	}
	return draftJobOutput(draft, nil), nil
}

var terminalEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-_])`)
var commonCredential = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic)?\s*|(?:api[_-]?key|access[_-]?token|password|secret)\s*[:=]\s*)[^\s,;]+`)

func sanitizeGenerationLog(content string, secrets []string) string {
	content = terminalEscape.ReplaceAllString(content, "")
	content = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		if r == 127 {
			return -1
		}
		return r
	}, content)
	for _, secret := range secrets {
		if secret != "" {
			content = strings.ReplaceAll(content, secret, "[REDACTED]")
		}
	}
	return commonCredential.ReplaceAllString(content, "${1}[REDACTED]")
}

func sensitiveValues(value any, sensitive bool, out *[]string) {
	switch v := value.(type) {
	case map[string]any:
		marked, _ := v["sensitive"].(bool)
		writeOnly, _ := v["writeOnly"].(bool)
		for key, child := range v {
			lower := strings.ToLower(key)
			sensitiveValues(child, sensitive || marked || writeOnly || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "api_key"), out)
		}
	case []any:
		for _, child := range v {
			sensitiveValues(child, sensitive, out)
		}
	case float64:
		if sensitive {
			*out = append(*out, strconv.FormatFloat(v, 'f', -1, 64))
		}
	case string:
		if sensitive && v != "" {
			*out = append(*out, v)
		}
	}
}

func (m *Module) selectedGenerationLogs(ctx context.Context, draft *recipebuilder.Draft, selections []runExcerptSelection) ([]recipeassistant.RunExcerpt, error) {
	if len(selections) > 4 {
		return nil, invalidSelection("at most four log excerpts may be selected")
	}
	if len(selections) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{}
	if draft.ChangeContext != nil {
		allowed[draft.ChangeContext.BaseRecipeDigest] = true
	}
	current := draft
	seen := map[string]bool{}
	for current != nil && !seen[current.ID] {
		seen[current.ID] = true
		if current.State == "installed" && current.PackageDigest != "" {
			allowed[current.PackageDigest] = true
		}
		if current.ParentDraftID == "" {
			break
		}
		var err error
		current, err = m.env.RecipeBuilder.Get(ctx, current.ParentDraftID)
		if err != nil {
			return nil, err
		}
	}
	delete(allowed, "")
	secrets, err := m.env.Q.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	redactions := []string{}
	for _, secret := range secrets {
		if m.env.Secrets == nil {
			return nil, invalidSelection("credential redaction is unavailable")
		}
		value, err := m.env.Secrets.Open(secret.ID, 1, secret.Nonce, secret.Ciphertext)
		if err != nil {
			return nil, invalidSelection("credential redaction could not open a saved secret")
		}
		redactions = append(redactions, value)
		var structured any
		if json.Unmarshal([]byte(value), &structured) == nil {
			sensitiveValues(structured, true, &redactions)
		}
	}
	var manifest any
	_ = json.Unmarshal(draft.Manifest, &manifest)
	sensitiveValues(manifest, false, &redactions)
	var baseManifest any
	if draft.ChangeContext != nil {
		_ = json.Unmarshal(draft.ChangeContext.BaseManifest, &baseManifest)
		sensitiveValues(baseManifest, false, &redactions)
	}
	total := 0
	excerpts := make([]recipeassistant.RunExcerpt, 0, len(selections))
	for _, selected := range selections {
		total += selected.ByteCount
		if selected.ByteCount <= 0 || selected.ByteCount > 16<<10 || total > 32<<10 || selected.Rank < 0 || (selected.Stream != "stdout" && selected.Stream != "stderr") {
			return nil, invalidSelection("log selection exceeds bounds or has an invalid rank/stream")
		}
		deployment, err := m.env.Deploy.Get(ctx, selected.DeploymentID)
		if err != nil {
			return nil, invalidSelection("selected log deployment is unavailable")
		}
		if !allowed[deployment.RecipeDigest] {
			return nil, invalidSelection("selected deployment is unrelated to this saved recipe")
		}
		run, err := m.env.Runs.Get(ctx, selected.RunID)
		if err != nil || run.Kind != "serve" || run.DeploymentID == nil || *run.DeploymentID != selected.DeploymentID {
			return nil, invalidSelection("selected run does not belong to this deployment")
		}
		if digest, ok := run.Input["recipe_digest"].(string); ok && !allowed[digest] {
			return nil, invalidSelection("selected run used a different recipe version")
		}
		rankFound := false
		for _, placement := range deployment.Placements {
			if placement.Rank == selected.Rank {
				rankFound = true
			}
		}
		if !rankFound {
			return nil, invalidSelection("selected log rank does not belong to the deployment")
		}
		sensitiveValues(deployment.Settings, false, &redactions)
		for _, document := range []any{manifest, baseManifest} {
			if object, ok := document.(map[string]any); ok {
				if parameters, ok := object["parameters"].([]any); ok {
					for _, item := range parameters {
						parameter, ok := item.(map[string]any)
						if !ok {
							continue
						}
						marked, _ := parameter["sensitive"].(bool)
						if marked {
							name, _ := parameter["name"].(string)
							sensitiveValues(deployment.Settings[name], true, &redactions)
						}
					}
				}
			}
		}
		chunk, _, _, err := m.env.Runs.ReadLog(selected.RunID, selected.DeploymentID, selected.Rank, selected.Stream, selected.Offset, selected.ByteCount)
		if err != nil {
			return nil, err
		}
		rawDigest := sha256.Sum256(chunk)
		excerpts = append(excerpts, recipeassistant.RunExcerpt{RunID: selected.RunID, DeploymentID: selected.DeploymentID, Rank: selected.Rank, Stream: selected.Stream, Offset: selected.Offset, ByteCount: selected.ByteCount, SourceSHA256: hex.EncodeToString(rawDigest[:]), Content: sanitizeGenerationLog(string(chunk), redactions)})
	}
	return excerpts, nil
}
