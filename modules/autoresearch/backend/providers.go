package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (m *Module) preflightLMWProvider(ctx context.Context, endpoint, model string) error {
	requestBody := map[string]any{
		"model":  model,
		"input":  "Call the lmw_probe tool exactly once with value ok.",
		"stream": true,
		"tools": []map[string]any{{
			"type": "function", "name": "lmw_probe", "description": "Disposable compatibility probe",
			"parameters": map[string]any{"type": "object", "required": []string{"value"}, "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		}},
		"tool_choice": "required",
	}
	encoded, _ := json.Marshal(requestBody)
	url := strings.TrimRight(endpoint, "/") + "/responses"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer lmw-local")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("autoresearch.provider_unavailable: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("autoresearch.provider_incompatible: status %d", response.StatusCode)
	}
	text := string(body)
	if !strings.Contains(text, "lmw_probe") || (!strings.Contains(text, "function_call") && !strings.Contains(text, "tool_call")) || !strings.Contains(text, "data:") {
		return errors.New("autoresearch.provider_incompatible")
	}
	return nil
}

func (m *Module) resolveProviderMap(ctx context.Context, providers map[string]any) error {
	for _, raw := range providers {
		provider, ok := raw.(map[string]any)
		if !ok || provider["source"] != "lmw" {
			continue
		}
		deploymentID, _ := provider["deployment_id"].(string)
		model, _ := provider["model"].(string)
		if deploymentID == "" || model == "" {
			return errors.New("autoresearch.provider_incompatible")
		}
		deployment, err := m.env.Deploy.Get(ctx, deploymentID)
		if err != nil || deployment.ObservedState != "healthy" || deployment.Endpoint == nil {
			return errors.New("autoresearch.provider_unavailable")
		}
		endpoint := fmt.Sprintf("http://%s:%d%s", deployment.Endpoint.Host, deployment.Endpoint.Port, deployment.Endpoint.Path)
		if err := m.preflightLMWProvider(ctx, endpoint, model); err != nil {
			return err
		}
		provider["backend"] = "codex"
		provider["endpoint"] = endpoint
		provider["base_url"] = endpoint
	}
	return nil
}

func (m *Module) resolveProjectProviders(ctx context.Context, config map[string]any) error {
	if roles, ok := config["roles"].(map[string]any); ok {
		if err := m.resolveProviderMap(ctx, roles); err != nil {
			return err
		}
	}
	if fallbacks, ok := config["fallbacks"].(map[string]any); ok {
		for _, rawList := range fallbacks {
			list, _ := rawList.([]any)
			for _, raw := range list {
				provider, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if err := m.resolveProviderMap(ctx, map[string]any{"fallback": provider}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
