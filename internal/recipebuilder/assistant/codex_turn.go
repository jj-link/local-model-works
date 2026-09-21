package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

func (c *Codex) Generate(ctx context.Context, req Request, progress func(string)) (Result, error) {
	if !c.turnMu.TryLock() {
		return Result{}, &Error{Code: "assistant.busy", Message: "another Codex investigation or turn is active", Retryable: true}
	}
	defer c.turnMu.Unlock()
	return generate(ctx, req, progress, c.complete)
}

func (c *Codex) complete(ctx context.Context, req Request, system string, schema map[string]any, progress func(string)) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, providerTurnTimeout)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	req.ResultSchema = schema
	approved, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var threadResponse struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.request(ctx, "thread/start", map[string]any{"model": req.Model}, &threadResponse); err != nil {
		return nil, err
	}
	if threadResponse.Thread.ID == "" {
		return nil, &Error{Code: "assistant.codex_protocol", Message: "Codex did not return a thread ID", Retryable: false}
	}
	input := system + "\n\n" + string(approved)
	var turnResponse struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	params := map[string]any{
		"threadId":       threadResponse.Thread.ID,
		"input":          []map[string]string{{"type": "text", "text": input}},
		"model":          req.Model,
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly"},
		"outputSchema":   schema,
	}
	if err := c.request(ctx, "turn/start", params, &turnResponse); err != nil {
		return nil, err
	}
	if turnResponse.Turn.ID == "" {
		return nil, &Error{Code: "assistant.codex_protocol", Message: "Codex did not return a turn ID", Retryable: false}
	}
	if progress != nil {
		progress("waiting_for_model")
	}
	var finalText string
	for {
		select {
		case <-ctx.Done():
			interruptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.request(interruptCtx, "turn/interrupt", map[string]string{
				"threadId": threadResponse.Thread.ID, "turnId": turnResponse.Turn.ID,
			}, nil)
			cancel()
			return nil, ctx.Err()
		case message := <-c.turnNotices:
			switch message.Method {
			case "process/stopped":
				return nil, &Error{Code: "assistant.codex_interrupted", Message: "Codex app-server stopped during the turn; the turn was not replayed", Retryable: true}
			case "item/completed":
				var completed struct {
					ThreadID string `json:"threadId"`
					TurnID   string `json:"turnId"`
					Item     struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"item"`
				}
				if json.Unmarshal(message.Params, &completed) == nil && completed.ThreadID == threadResponse.Thread.ID &&
					completed.TurnID == turnResponse.Turn.ID && completed.Item.Type == "agentMessage" {
					finalText = completed.Item.Text
				}
			case "turn/completed":
				var completed struct {
					ThreadID string `json:"threadId"`
					Turn     struct {
						ID     string `json:"id"`
						Status string `json:"status"`
						Error  *struct {
							Message string `json:"message"`
						} `json:"error"`
					} `json:"turn"`
				}
				if json.Unmarshal(message.Params, &completed) != nil || completed.ThreadID != threadResponse.Thread.ID || completed.Turn.ID != turnResponse.Turn.ID {
					continue
				}
				if completed.Turn.Status != "completed" {
					message := "Codex turn did not complete"
					if completed.Turn.Error != nil && strings.TrimSpace(completed.Turn.Error.Message) != "" {
						message = strings.TrimSpace(completed.Turn.Error.Message)
					}
					return nil, &Error{Code: "assistant.codex_interrupted", Message: message, Retryable: true}
				}
				if finalText == "" {
					return nil, invalid("assistant.provider_invalid_json", "Codex returned no final JSON")
				}
				return []byte(finalText), nil
			}
		}
	}
}
