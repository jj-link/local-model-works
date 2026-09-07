package assistant

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

// Probe performs a small text-only completion against the configured provider.
func (p *OpenAICompatible) Probe(ctx context.Context) (string, error) {
	endpoint, err := p.completionURL()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Model) == "" {
		return "", invalid("assistant.model_required", "provider model is required")
	}
	body, err := json.Marshal(completionRequest{
		Model:    p.Model,
		Messages: []completionMessage{{Role: "user", Content: "Reply with exactly: OK"}},
		Stream:   false,
	})
	if err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", invalid("assistant.provider_invalid", "provider endpoint is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	if p.SecretID != "" {
		if p.ResolveSecret == nil {
			return "", invalid("assistant.secret_unavailable", "provider credential resolver is unavailable")
		}
		secret, resolveErr := p.ResolveSecret(probeCtx, p.SecretID)
		if resolveErr != nil {
			return "", &Error{Code: "assistant.secret_unavailable", Message: "selected provider credential is unavailable", Retryable: false}
		}
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	client := providerClient(p.Client)
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return "", &Error{Code: "assistant.provider_timeout", Message: "provider connection test timed out", Retryable: true}
		}
		if errors.Is(probeCtx.Err(), context.Canceled) {
			return "", probeCtx.Err()
		}
		return "", &Error{Code: "assistant.provider_unavailable", Message: "provider endpoint is unavailable", Retryable: true}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", &Error{Code: "assistant.provider_auth", Message: "provider rejected the selected credential", Retryable: false}
	case http.StatusTooManyRequests:
		return "", &Error{Code: "assistant.provider_rate_limited", Message: "provider rate limit or quota was reached", Retryable: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &Error{Code: "assistant.provider_unavailable", Message: fmt.Sprintf("provider returned HTTP %d", response.StatusCode), Retryable: response.StatusCode >= 500}
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(payload) > MaxResponseBytes {
		return "", &Error{Code: "assistant.provider_truncated", Message: "provider connection response was incomplete", Retryable: err != nil}
	}
	var completion completionResponse
	if json.Unmarshal(payload, &completion) != nil || len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return "", &Error{Code: "assistant.provider_invalid_json", Message: "provider returned an invalid completion envelope", Retryable: false}
	}
	return strings.TrimSpace(completion.Choices[0].Message.Content), nil
}
