package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/httpx"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

type assistantProviderSettings struct {
	Assistant struct {
		Providers []assistantProviderConfig `json:"providers"`
	} `json:"assistant"`
}

type assistantProviderConfig struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Kind           string `json:"kind"`
	DeploymentID   string `json:"deployment_id"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKeySecretID string `json:"api_key_secret_id"`
}

func decodeAssistantSettings(stored map[string]any) (assistantProviderSettings, error) {
	var settings assistantProviderSettings
	encoded, err := json.Marshal(stored)
	if err != nil {
		return settings, err
	}
	err = json.Unmarshal(encoded, &settings)
	return settings, err
}

// assistantProviders is the shared catalog for discovery and selection. Saved
// local/Codex profiles are deliberately ignored: those identities belong to the
// deployment service and connected account, not settings.
func (m *Module) assistantProviders(ctx context.Context, stored map[string]any) ([]assistantProviderConfig, error) {
	settings, err := decodeAssistantSettings(stored)
	if err != nil {
		return nil, err
	}
	providers := make([]assistantProviderConfig, 0)
	if m.env.Deploy != nil {
		// Service.List omits view errors; enumerate rows so discovery failures
		// cannot silently turn a real provider into an empty catalog.
		deployments, err := m.env.Q.ListDeployments(ctx)
		if err != nil {
			return nil, err
		}
		for _, deployment := range deployments {
			if deployment.DesiredState != "running" || deployment.ObservedState != "healthy" {
				continue
			}
			endpoint, err := m.resolveEnrolledLocal(ctx, deployment.ID)
			if err != nil {
				var typed *recipeassistant.Error
				if errors.As(err, &typed) && (typed.Code == "assistant.deployment_not_ready" || typed.Code == "assistant.endpoint_unavailable" || typed.Code == "assistant.deployment_unknown") {
					continue
				}
				return nil, err
			}
			providers = append(providers, assistantProviderConfig{ID: "local:" + deployment.ID, Label: endpoint.Model + " (local)", Kind: "local", DeploymentID: deployment.ID, BaseURL: endpoint.URL, Model: endpoint.Model})
		}
	}
	if m.env.RecipeAssistant != nil {
		account, err := m.env.RecipeAssistant.Account(ctx)
		if err != nil {
			var typed *recipeassistant.Error
			if !errors.As(err, &typed) || typed.Code != "assistant.codex_missing" {
				return nil, err
			}
		} else if account.Connected {
			models, err := m.env.RecipeAssistant.Models(ctx)
			if err != nil {
				return nil, err
			}
			for _, model := range models {
				label := model.DisplayName
				if label == "" {
					label = model.ID
				}
				providers = append(providers, assistantProviderConfig{ID: "codex:" + model.ID, Label: label + " (Codex)", Kind: "codex", BaseURL: "codex://saved-account", Model: model.ID})
			}
		}
	}
	for _, provider := range settings.Assistant.Providers {
		if provider.Kind != "openai_compatible" {
			continue
		}
		// Reserved prefixes must never let a saved endpoint impersonate a
		// server-owned deployment or account model, even when unavailable.
		if provider.ID == "" || strings.HasPrefix(provider.ID, "local:") || strings.HasPrefix(provider.ID, "codex:") {
			return nil, &recipeassistant.Error{Code: "assistant.provider_invalid", Message: "remote provider ID is empty or uses a reserved prefix"}
		}
		for _, existing := range providers {
			if existing.ID == provider.ID {
				return nil, &recipeassistant.Error{Code: "assistant.provider_invalid", Message: "remote provider IDs must be unique"}
			}
		}
		if err := recipeassistant.ValidateEndpoint(provider.BaseURL); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

func selectAssistantProvider(providers []assistantProviderConfig, id string) (assistantProviderConfig, error) {
	for _, provider := range providers {
		if provider.ID == id {
			return provider, nil
		}
	}
	return assistantProviderConfig{}, &recipeassistant.Error{Code: "assistant.provider_unknown", Message: "selected provider is unknown or unavailable"}
}

func (m *Module) listRecipeAssistantProviders(w http.ResponseWriter, r *http.Request) {
	stored, version, err := m.env.Settings.Get(r.Context(), "library")
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	providers, err := m.assistantProviders(r.Context(), stored)
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	// Keep credentials out of the response, including encrypted secret IDs.
	type publicProvider struct {
		ID           string `json:"id"`
		Label        string `json:"label"`
		Kind         string `json:"kind"`
		DeploymentID string `json:"deployment_id,omitempty"`
		BaseURL      string `json:"base_url,omitempty"`
		Model        string `json:"model,omitempty"`
	}
	public := make([]publicProvider, 0, len(providers))
	for _, provider := range providers {
		public = append(public, publicProvider{ID: provider.ID, Label: provider.Label, Kind: provider.Kind, DeploymentID: provider.DeploymentID, BaseURL: provider.BaseURL, Model: provider.Model})
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"version": version, "providers": public})
}

func (m *Module) testRecipeAssistantProvider(w http.ResponseWriter, r *http.Request, providerID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stored, _, err := m.env.Settings.Get(r.Context(), "library")
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	providers, err := m.assistantProviders(ctx, stored)
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	selected, err := selectAssistantProvider(providers, providerID)
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	model := selected.Model
	switch selected.Kind {
	case "local":
		endpoint, resolveErr := m.resolveEnrolledLocal(ctx, selected.DeploymentID)
		if resolveErr != nil {
			writeAssistantError(w, resolveErr)
			return
		}
		model = endpoint.Model
		provider, providerErr := recipeassistant.LocalProvider(endpoint)
		if providerErr != nil {
			writeAssistantError(w, providerErr)
			return
		}
		_, err = provider.Probe(ctx)
	case "openai_compatible":
		provider := &recipeassistant.OpenAICompatible{
			BaseURL: selected.BaseURL, Model: selected.Model, SecretID: selected.APIKeySecretID,
			ResolveSecret: recipeassistant.EncryptedSecretResolver(m.env.Q, m.env.Secrets),
		}
		_, err = provider.Probe(ctx)
	case "codex":
		// Catalog discovery already checked the account and model availability.
	default:
		err = &recipeassistant.Error{Code: "assistant.provider_invalid", Message: "provider kind is invalid", Retryable: false}
	}
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "model": model, "message": "Connection succeeded"})
}

func (m *Module) getRecipeAssistantCodexStatus(w http.ResponseWriter, r *http.Request) {
	status, err := m.env.RecipeAssistant.Account(r.Context())
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (m *Module) startRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request) {
	login, err := m.env.RecipeAssistant.StartDeviceLogin(r.Context())
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, login)
}

func (m *Module) getRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request) {
	status, err := m.env.RecipeAssistant.DeviceLoginStatus(chi.URLParam(r, "id"))
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, status)
}

func (m *Module) cancelRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request) {
	if err := m.env.RecipeAssistant.CancelDeviceLogin(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeAssistantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) logoutRecipeAssistantCodex(w http.ResponseWriter, r *http.Request) {
	if err := m.env.RecipeAssistant.Logout(r.Context()); err != nil {
		writeAssistantError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) listRecipeAssistantCodexModels(w http.ResponseWriter, r *http.Request) {
	models, err := m.env.RecipeAssistant.Models(r.Context())
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, models)
}

func writeAssistantError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "assistant.unavailable"
	message := "assistant provider is unavailable"
	var typed *recipeassistant.Error
	if errors.As(err, &typed) {
		code, message = typed.Code, typed.Message
		if strings.Contains(code, "unknown") || strings.Contains(code, "expired") {
			status = http.StatusNotFound
		} else if strings.Contains(code, "invalid") || strings.Contains(code, "required") || strings.Contains(code, "purpose") {
			status = http.StatusBadRequest
		} else if strings.Contains(code, "timeout") {
			status = http.StatusGatewayTimeout
		} else if strings.Contains(code, "busy") || strings.Contains(code, "active") {
			status = http.StatusConflict
		}
	} else if errors.Is(err, context.DeadlineExceeded) {
		status, code, message = http.StatusGatewayTimeout, "assistant.provider_timeout", "provider connection test timed out"
	}
	retryable := false
	if typed != nil {
		retryable = typed.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		retryable = true
	}
	httpx.WriteJSON(w, status, httpx.Error{Code: code, Message: message, Details: map[string]any{"retryable": retryable}})
}
