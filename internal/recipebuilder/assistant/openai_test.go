package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAICompatibleUsesSelectedSecretAndParsesBoundedProposal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer encrypted-secret" {
			t.Fatalf("request path=%q authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		proposal, _ := json.Marshal(Result{Manifest: json.RawMessage(`{}`), Files: []File{{Path: "start.sh", Content: "echo safe\n"}}})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(proposal)}}}})
	}))
	defer server.Close()
	provider := &OpenAICompatible{
		BaseURL: server.URL + "/v1", Model: "selected-model", SecretID: "secret-id",
		ResolveSecret: func(_ context.Context, id string) (string, error) {
			if id != "secret-id" {
				t.Fatalf("secret id = %q", id)
			}
			return "encrypted-secret", nil
		},
	}
	result, err := provider.Generate(context.Background(), Request{Manifest: json.RawMessage(`{}`)}, nil)
	if err != nil || len(result.Files) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestOpenAICompatibleProbeUsesNoRepositoryContext(t *testing.T) {
	const secret = "probe-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(request)
		if strings.Contains(string(encoded), "repository") || len(request.Messages) != 1 || request.Messages[0].Content != "Reply with exactly: OK" {
			t.Fatalf("probe request = %s", encoded)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "OK"}}}})
	}))
	defer server.Close()
	provider := &OpenAICompatible{
		BaseURL: server.URL + "/v1", Model: "selected-model", SecretID: "selected-secret",
		ResolveSecret: func(context.Context, string) (string, error) { return secret, nil },
	}
	answer, err := provider.Probe(context.Background())
	if err != nil || answer != "OK" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
}

func TestOpenAICompatibleMapsAuthRateLimitAndInvalidJSON(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "auth", status: http.StatusUnauthorized, code: "assistant.provider_auth"},
		{name: "rate", status: http.StatusTooManyRequests, code: "assistant.provider_rate_limited"},
		{name: "invalid", status: http.StatusOK, body: `{"choices":[{"message":{"content":"not-json"}}]}`, code: "assistant.provider_invalid_json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := (&OpenAICompatible{BaseURL: server.URL + "/v1", Model: "model"}).Generate(context.Background(), Request{}, nil)
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCompletionURLRejectsCredentialsPublicHTTPAndRedirects(t *testing.T) {
	for _, value := range []string{"http://example.com/v1", "https://user:secret@example.com/v1", "https://example.com/v1?q=secret"} {
		if _, err := completionURL(value); err == nil {
			t.Fatalf("unsafe URL accepted: %s", value)
		}
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, httptest.NewRequest(http.MethodGet, "/", nil), "https://example.com", http.StatusFound)
	}))
	defer redirect.Close()
	provider := &OpenAICompatible{BaseURL: redirect.URL, Model: "model", SecretID: "id", ResolveSecret: func(context.Context, string) (string, error) { return "do-not-forward", nil }}
	_, err := provider.Generate(context.Background(), Request{}, nil)
	if err == nil || strings.Contains(err.Error(), "do-not-forward") {
		t.Fatalf("redirect error = %v", err)
	}
}
