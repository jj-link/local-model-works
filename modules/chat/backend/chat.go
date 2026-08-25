package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/httpx"
)

const (
	requestBodyLimit     = int64(1 << 20)
	upstreamErrorLimit   = int64(64 << 10)
	upstreamSuccessLimit = int64(4 << 20)
)

var chatUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	DeploymentID string        `json:"deployment_id"`
	Messages     []chatMessage `json:"messages"`
	Temperature  *float64      `json:"temperature,omitempty"`
	MaxTokens    *int          `json:"max_tokens,omitempty"`
}

type upstreamRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

type assistantMessage struct {
	Role             string  `json:"role"`
	Content          string  `json:"content"`
	ReasoningContent *string `json:"reasoning_content,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID           string           `json:"id"`
	Model        string           `json:"model"`
	Message      assistantMessage `json:"message"`
	FinishReason *string          `json:"finish_reason,omitempty"`
	Usage        *chatUsage       `json:"usage,omitempty"`
}

type upstreamResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          string  `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

func (m *Module) completions(w http.ResponseWriter, r *http.Request) {
	req, err := decodeChatRequest(w, r)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if err := validateChatRequest(req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}

	deployment, err := m.getDeployment(r.Context(), req.DeploymentID)
	if err != nil {
		if errors.Is(err, deploy.ErrUnknown) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "deployment not found")
			return
		}
		httpx.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if deployment.DesiredState != "running" || deployment.ObservedState != "healthy" {
		httpx.WriteErr(w, http.StatusConflict, "chat.deployment_not_ready", "deployment must be running and healthy")
		return
	}
	if deployment.Endpoint == nil || strings.TrimSpace(deployment.Endpoint.Host) == "" || deployment.Endpoint.Port <= 0 || strings.TrimSpace(deployment.Endpoint.Model) == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "chat.endpoint_unavailable", "deployment endpoint host, port, and model are required")
		return
	}

	body, err := json.Marshal(upstreamRequest{
		Model:       deployment.Endpoint.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      false,
	})
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	endpoint := "http://" + net.JoinHostPort(deployment.Endpoint.Host, strconv.Itoa(int(deployment.Endpoint.Port))) + "/v1/chat/completions"
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(upstream)
	if err != nil {
		if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		if isTimeout(err) {
			httpx.WriteErr(w, http.StatusGatewayTimeout, "chat.upstream_timeout", "deployment endpoint timed out")
			return
		}
		httpx.WriteErr(w, http.StatusBadGateway, "chat.upstream_unavailable", "deployment endpoint is unavailable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorLimit))
		httpx.WriteErr(w, http.StatusBadGateway, "chat.upstream_unavailable", upstreamFailureMessage(resp.Status, errBody))
		return
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, upstreamSuccessLimit+1))
	if err != nil || int64(len(payload)) > upstreamSuccessLimit {
		httpx.WriteErr(w, http.StatusBadGateway, "chat.upstream_unavailable", "deployment endpoint returned an invalid response")
		return
	}
	var decoded upstreamResponse
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.ID == "" || decoded.Model == "" || len(decoded.Choices) == 0 {
		httpx.WriteErr(w, http.StatusBadGateway, "chat.upstream_unavailable", "deployment endpoint returned an invalid response")
		return
	}
	choice := decoded.Choices[0]
	httpx.WriteJSON(w, http.StatusOK, chatResponse{
		ID:    decoded.ID,
		Model: decoded.Model,
		Message: assistantMessage{
			Role:             "assistant",
			Content:          choice.Message.Content,
			ReasoningContent: choice.Message.ReasoningContent,
		},
		FinishReason: choice.FinishReason,
		Usage:        decoded.Usage,
	})
}

func decodeChatRequest(w http.ResponseWriter, r *http.Request) (chatRequest, error) {
	var req chatRequest
	r.Body = http.MaxBytesReader(w, r.Body, requestBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid JSON body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return req, fmt.Errorf("invalid JSON body: expected one object")
	}
	return req, nil
}

func validateChatRequest(req chatRequest) error {
	if !chatUUIDPattern.MatchString(req.DeploymentID) {
		return fmt.Errorf("deployment_id must be a UUID")
	}
	if len(req.Messages) < 1 || len(req.Messages) > 64 {
		return fmt.Errorf("messages must contain between 1 and 64 items")
	}
	for i, message := range req.Messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			return fmt.Errorf("messages[%d].role is invalid", i)
		}
		if message.Content == "" || utf8.RuneCountInString(message.Content) > 65536 {
			return fmt.Errorf("messages[%d].content must contain 1 to 65536 characters", i)
		}
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if req.MaxTokens != nil && (*req.MaxTokens < 1 || *req.MaxTokens > 32768) {
		return fmt.Errorf("max_tokens must be between 1 and 32768")
	}
	return nil
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func upstreamFailureMessage(status string, body []byte) string {
	message := "deployment endpoint returned " + status
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		detail := envelope.Error.Message
		if len(detail) > 512 {
			detail = detail[:512]
		}
		message += ": " + detail
	}
	return message
}
