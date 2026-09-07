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
		DefaultProviderID string                    `json:"default_provider_id"`
		Providers         []assistantProviderConfig `json:"providers"`
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

func findAssistantProvider(settings assistantProviderSettings, id string) *assistantProviderConfig {
	for i := range settings.Assistant.Providers {
		if settings.Assistant.Providers[i].ID == id {
			return &settings.Assistant.Providers[i]
		}
	}
	return nil
}

func (m *Module) testRecipeAssistantProvider(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "id")
	stored, _, err := m.env.Settings.Get(r.Context(), "library")
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	settings, err := decodeAssistantSettings(stored)
	if err != nil {
		writeAssistantError(w, err)
		return
	}
	selected := findAssistantProvider(settings, providerID)
	if selected == nil {
		httpx.WriteJSON(w, http.StatusNotFound, httpx.Error{Code: "assistant.provider_unknown", Message: "saved provider does not exist"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
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
		if m.env.RecipeAssistant == nil {
			err = &recipeassistant.Error{Code: "assistant.codex_unavailable", Message: "Codex provider is unavailable", Retryable: true}
			break
		}
		_, err = m.env.RecipeAssistant.Account(ctx)
		if err == nil {
			models, modelsErr := m.env.RecipeAssistant.Models(ctx)
			err = modelsErr
			if model == "" && len(models) != 0 {
				model = models[0].ID
			}
		}
	default:
		err = &recipeassistant.Error{Code: "assistant.provider_invalid", Message: "saved provider kind is invalid", Retryable: false}
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
