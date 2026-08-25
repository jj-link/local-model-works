package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
)

const testDeploymentID = "11111111-1111-4111-8111-111111111111"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func readyDeployment(t *testing.T, rawURL string) *deploy.Deployment {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return &deploy.Deployment{
		ID:            testDeploymentID,
		DesiredState:  "running",
		ObservedState: "healthy",
		Endpoint:      &deploy.Endpoint{Host: host, Port: int32(port), Model: "forced-model"},
	}
}

func stubModule(deployment *deploy.Deployment, getErr error, client *http.Client) *Module {
	if client == nil {
		client = http.DefaultClient
	}
	return &Module{
		getDeployment: func(context.Context, string) (*deploy.Deployment, error) {
			return deployment, getErr
		},
		client: client,
	}
}

func completionRequest(t *testing.T, ctx context.Context, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(body))
	return req.WithContext(ctx)
}

func responseCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error response %q: %v", recorder.Body.String(), err)
	}
	return envelope.Code
}

func TestChatCompletionForwardsResolvedModel(t *testing.T) {
	var upstreamBody struct {
		Model       string        `json:"model"`
		Messages    []chatMessage `json:"messages"`
		Temperature *float64      `json:"temperature"`
		MaxTokens   *int          `json:"max_tokens"`
		Stream      bool          `json:"stream"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","model":"served-model","choices":[{"message":{"content":"real answer","reasoning_content":"real reasoning"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	}))
	defer upstream.Close()

	module := stubModule(readyDeployment(t, upstream.URL), nil, upstream.Client())
	recorder := httptest.NewRecorder()
	module.completions(recorder, completionRequest(t, context.Background(), `{"deployment_id":"`+testDeploymentID+`","messages":[{"role":"user","content":"hello"}],"temperature":0.25,"max_tokens":128}`))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if upstreamBody.Model != "forced-model" || upstreamBody.Stream {
		t.Fatalf("upstream model=%q stream=%v", upstreamBody.Model, upstreamBody.Stream)
	}
	if len(upstreamBody.Messages) != 1 || upstreamBody.Messages[0].Content != "hello" || upstreamBody.Temperature == nil || *upstreamBody.Temperature != 0.25 || upstreamBody.MaxTokens == nil || *upstreamBody.MaxTokens != 128 {
		t.Fatalf("upstream body = %+v", upstreamBody)
	}
	var got chatResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "chat-1" || got.Model != "served-model" || got.Message.Role != "assistant" || got.Message.Content != "real answer" || got.Message.ReasoningContent == nil || *got.Message.ReasoningContent != "real reasoning" || got.Usage == nil || got.Usage.TotalTokens != 10 {
		t.Fatalf("completion response = %+v", got)
	}
}

func TestChatCompletionDeploymentErrors(t *testing.T) {
	tests := []struct {
		name       string
		deployment *deploy.Deployment
		getErr     error
		status     int
		code       string
	}{
		{name: "unknown", getErr: deploy.ErrUnknown, status: http.StatusNotFound, code: "resource.not_found"},
		{name: "unhealthy", deployment: &deploy.Deployment{DesiredState: "running", ObservedState: "degraded"}, status: http.StatusConflict, code: "chat.deployment_not_ready"},
		{name: "stopped", deployment: &deploy.Deployment{DesiredState: "stopped", ObservedState: "healthy"}, status: http.StatusConflict, code: "chat.deployment_not_ready"},
		{name: "missing endpoint", deployment: &deploy.Deployment{DesiredState: "running", ObservedState: "healthy"}, status: http.StatusUnprocessableEntity, code: "chat.endpoint_unavailable"},
		{name: "missing model", deployment: &deploy.Deployment{DesiredState: "running", ObservedState: "healthy", Endpoint: &deploy.Endpoint{Host: "127.0.0.1", Port: 8000}}, status: http.StatusUnprocessableEntity, code: "chat.endpoint_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			stubModule(tt.deployment, tt.getErr, nil).completions(recorder, completionRequest(t, context.Background(), `{"deployment_id":"`+testDeploymentID+`","messages":[{"role":"user","content":"hello"}]}`))
			if recorder.Code != tt.status || responseCode(t, recorder) != tt.code {
				t.Fatalf("status=%d code=%q body=%s", recorder.Code, responseCode(t, recorder), recorder.Body.String())
			}
		})
	}
}

func TestChatCompletionUpstreamFailures(t *testing.T) {
	ready := &deploy.Deployment{
		DesiredState:  "running",
		ObservedState: "healthy",
		Endpoint:      &deploy.Endpoint{Host: "127.0.0.1", Port: 8000, Model: "model"},
	}
	tests := []struct {
		name   string
		client *http.Client
		status int
		code   string
	}{
		{
			name: "connect",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			})},
			status: http.StatusBadGateway,
			code:   "chat.upstream_unavailable",
		},
		{
			name: "non-2xx",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: io.NopCloser(strings.NewReader(`{"error":{"message":"engine failed"}}`)), Header: make(http.Header)}, nil
			})},
			status: http.StatusBadGateway,
			code:   "chat.upstream_unavailable",
		},
		{
			name: "invalid JSON",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`not-json`)), Header: make(http.Header)}, nil
			})},
			status: http.StatusBadGateway,
			code:   "chat.upstream_unavailable",
		},
		{
			name: "oversized success",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(upstreamSuccessLimit+1)))), Header: make(http.Header)}, nil
			})},
			status: http.StatusBadGateway,
			code:   "chat.upstream_unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			stubModule(ready, nil, tt.client).completions(recorder, completionRequest(t, context.Background(), `{"deployment_id":"`+testDeploymentID+`","messages":[{"role":"user","content":"hello"}]}`))
			if recorder.Code != tt.status || responseCode(t, recorder) != tt.code {
				t.Fatalf("status=%d code=%q body=%s", recorder.Code, responseCode(t, recorder), recorder.Body.String())
			}
		})
	}
}

func TestChatCompletionTimeoutAndCancellation(t *testing.T) {
	ready := &deploy.Deployment{
		DesiredState:  "running",
		ObservedState: "healthy",
		Endpoint:      &deploy.Endpoint{Host: "127.0.0.1", Port: 8000, Model: "model"},
	}

	t.Run("timeout", func(t *testing.T) {
		client := &http.Client{
			Timeout: 20 * time.Millisecond,
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}),
		}
		recorder := httptest.NewRecorder()
		stubModule(ready, nil, client).completions(recorder, completionRequest(t, context.Background(), `{"deployment_id":"`+testDeploymentID+`","messages":[{"role":"user","content":"hello"}]}`))
		if recorder.Code != http.StatusGatewayTimeout || responseCode(t, recorder) != "chat.upstream_timeout" {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("operator cancellation", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return nil, r.Context().Err()
		})}
		ctx, cancel := context.WithCancel(context.Background())
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			stubModule(ready, nil, client).completions(recorder, completionRequest(t, ctx, `{"deployment_id":"`+testDeploymentID+`","messages":[{"role":"user","content":"hello"}]}`))
			close(done)
		}()
		<-started
		cancel()
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("upstream request context was not canceled")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler did not return after cancellation")
		}
	})
}

func TestChatCompletionRequestBounds(t *testing.T) {
	ready := &deploy.Deployment{
		DesiredState:  "running",
		ObservedState: "healthy",
		Endpoint:      &deploy.Endpoint{Host: "127.0.0.1", Port: 8000, Model: "model"},
	}
	messages := make([]string, 65)
	for i := range messages {
		messages[i] = `{"role":"user","content":"x"}`
	}
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid UUID", body: `{"deployment_id":"bad","messages":[{"role":"user","content":"x"}]}`},
		{name: "empty messages", body: `{"deployment_id":"` + testDeploymentID + `","messages":[]}`},
		{name: "too many messages", body: `{"deployment_id":"` + testDeploymentID + `","messages":[` + strings.Join(messages, ",") + `]}`},
		{name: "invalid role", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"tool","content":"x"}]}`},
		{name: "empty content", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":""}]}`},
		{name: "long content", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":"` + strings.Repeat("x", 65537) + `"}]}`},
		{name: "temperature", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":"x"}],"temperature":2.1}`},
		{name: "max tokens", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":"x"}],"max_tokens":0}`},
		{name: "unknown field", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":"x"}],"model":"client-model"}`},
		{name: "body over one MiB", body: `{"deployment_id":"` + testDeploymentID + `","messages":[{"role":"user","content":"` + strings.Repeat("x", int(requestBodyLimit)) + `"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			stubModule(ready, nil, nil).completions(recorder, completionRequest(t, context.Background(), tt.body))
			if recorder.Code != http.StatusUnprocessableEntity || responseCode(t, recorder) != "resource.unprocessable" {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
