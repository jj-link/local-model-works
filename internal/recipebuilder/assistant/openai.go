package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const providerTurnTimeout = 10 * time.Minute

type SecretResolver func(context.Context, string) (string, error)

type OpenAICompatible struct {
	BaseURL         string
	Model           string
	SecretID        string
	ResolveSecret   SecretResolver
	Client          *http.Client
	trustedLocalURL string
}

type completionRequest struct {
	Model               string              `json:"model"`
	Messages            []completionMessage `json:"messages"`
	Stream              bool                `json:"stream"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *completionFormat   `json:"response_format,omitempty"`
}

type completionFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string         `json:"name"`
		Schema map[string]any `json:"schema"`
	} `json:"json_schema"`
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (p *OpenAICompatible) Generate(ctx context.Context, req Request, progress func(string)) (Result, error) {
	return generate(ctx, req, progress, p.complete)
}

func (p *OpenAICompatible) complete(ctx context.Context, req Request, system string, schema map[string]any, progress func(string)) ([]byte, error) {
	endpoint, err := p.completionURL()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Model) == "" {
		return nil, invalid("assistant.model_required", "provider model is required")
	}
	req.ResultSchema = schema
	approved, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	input := completionRequest{
		Model: p.Model,
		Messages: []completionMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: string(approved)},
		},
		Stream:              false,
		MaxCompletionTokens: 32768,
	}
	input.ResponseFormat = &completionFormat{Type: "json_schema"}
	input.ResponseFormat.JSONSchema.Name = "recipe_response"
	input.ResponseFormat.JSONSchema.Schema = schema
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithTimeout(ctx, providerTurnTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(turnCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, invalid("assistant.provider_invalid", "provider endpoint is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	if p.SecretID != "" {
		if p.ResolveSecret == nil {
			return nil, invalid("assistant.secret_unavailable", "provider credential resolver is unavailable")
		}
		secret, resolveErr := p.ResolveSecret(turnCtx, p.SecretID)
		if resolveErr != nil {
			return nil, &Error{Code: "assistant.secret_unavailable", Message: "selected provider credential is unavailable", Retryable: false}
		}
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	if progress != nil {
		progress("waiting_for_model")
	}
	client := providerClient(p.Client)
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
			return nil, &Error{Code: "assistant.provider_timeout", Message: "provider request timed out", Retryable: true}
		}
		if errors.Is(turnCtx.Err(), context.Canceled) {
			return nil, turnCtx.Err()
		}
		return nil, &Error{Code: "assistant.provider_unavailable", Message: "provider endpoint is unavailable", Retryable: true}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, &Error{Code: "assistant.provider_auth", Message: "provider rejected the selected credential", Retryable: false}
	case http.StatusTooManyRequests:
		return nil, &Error{Code: "assistant.provider_rate_limited", Message: "provider rate limit or quota was reached", Retryable: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &Error{Code: "assistant.provider_unavailable", Message: fmt.Sprintf("provider returned HTTP %d", response.StatusCode), Retryable: response.StatusCode >= 500}
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, &Error{Code: "assistant.provider_truncated", Message: "provider response ended before a complete proposal was received", Retryable: true}
	}
	if len(payload) > MaxResponseBytes {
		return nil, &Error{Code: "assistant.provider_truncated", Message: "provider response exceeds the 4 MiB limit", Retryable: false}
	}
	var completion completionResponse
	if json.Unmarshal(payload, &completion) != nil || len(completion.Choices) != 1 {
		return nil, &Error{Code: "assistant.provider_invalid_json", Message: "provider returned an invalid completion envelope", Retryable: false}
	}
	if completion.Choices[0].FinishReason == "length" {
		return nil, &Error{Code: "assistant.provider_truncated", Message: "provider reached its output token limit before completing the JSON result; use a provider/model with a larger output allowance", Retryable: true}
	}
	if completion.Choices[0].Message.Content == "" {
		return nil, &Error{Code: "assistant.provider_invalid_json", Message: "provider returned an empty completion", Retryable: false}
	}
	return []byte(completion.Choices[0].Message.Content), nil
}

func (p *OpenAICompatible) completionURL() (string, error) {
	if p.trustedLocalURL != "" && p.BaseURL == p.trustedLocalURL && p.SecretID == "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", invalid("assistant.provider_invalid", "resolved local endpoint is invalid")
		}
		u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), "chat/completions")
		return u.String(), nil
	}
	return completionURL(p.BaseURL)
}

// ValidateEndpoint checks public provider configuration without network access.
func ValidateEndpoint(base string) error { _, err := completionURL(base); return err }

func completionURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("assistant.provider_invalid", "base_url must not contain credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return "", invalid("assistant.provider_invalid", "base_url must use HTTPS, except for loopback HTTP")
	}
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), "chat/completions")
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func rejectProviderRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func providerClient(configured *http.Client) *http.Client {
	client := http.Client{}
	if configured != nil {
		client = *configured
	}
	client.CheckRedirect = rejectProviderRedirect
	return &client
}
