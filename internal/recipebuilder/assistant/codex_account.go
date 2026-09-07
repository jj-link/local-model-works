package assistant

import (
	"context"
	"encoding/json"
	"strings"
)

type AccountStatus struct {
	Available bool   `json:"available"`
	Connected bool   `json:"connected"`
	Email     string `json:"email,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Error     string `json:"error,omitempty"`
}

type DeviceLogin struct {
	LoginID         string `json:"login_id"`
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
}

type LoginStatus struct {
	LoginID string `json:"login_id"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

type CodexModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default"`
}

func (c *Codex) Account(ctx context.Context) (AccountStatus, error) {
	if err := c.Start(ctx); err != nil {
		return AccountStatus{Error: err.Error()}, err
	}
	var response struct {
		Account *struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	if err := c.request(ctx, "account/read", map[string]bool{"refreshToken": false}, &response); err != nil {
		return AccountStatus{Available: true, Error: err.Error()}, err
	}
	status := AccountStatus{Available: true}
	if response.Account != nil && response.Account.Type == "chatgpt" {
		status.Connected = true
		status.Email = response.Account.Email
		status.Plan = response.Account.PlanType
	}
	return status, nil
}

func (c *Codex) StartDeviceLogin(ctx context.Context) (DeviceLogin, error) {
	if err := c.Start(ctx); err != nil {
		return DeviceLogin{}, err
	}
	c.stateMu.Lock()
	if c.activeLogin != "" {
		active := c.logins[c.activeLogin]
		if active.State == "pending" {
			c.stateMu.Unlock()
			return DeviceLogin{}, &Error{Code: "assistant.codex_login_active", Message: "a Codex login attempt is already pending", Retryable: false}
		}
	}
	c.stateMu.Unlock()
	var response struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := c.request(ctx, "account/login/start", map[string]string{"type": "chatgptDeviceCode"}, &response); err != nil {
		return DeviceLogin{}, err
	}
	if response.Type != "chatgptDeviceCode" || response.LoginID == "" || response.VerificationURL == "" || response.UserCode == "" {
		return DeviceLogin{}, &Error{Code: "assistant.codex_unsupported", Message: "Codex did not return a device-code login", Retryable: false}
	}
	c.stateMu.Lock()
	if c.logins == nil {
		c.logins = make(map[string]LoginStatus)
	}
	c.activeLogin = response.LoginID
	c.logins[response.LoginID] = LoginStatus{LoginID: response.LoginID, State: "pending"}
	c.stateMu.Unlock()
	return DeviceLogin{LoginID: response.LoginID, VerificationURL: response.VerificationURL, UserCode: response.UserCode}, nil
}

func (c *Codex) DeviceLoginStatus(loginID string) (LoginStatus, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	status, ok := c.logins[loginID]
	if !ok {
		return LoginStatus{}, &Error{Code: "assistant.codex_login_expired", Message: "login attempt is unknown or expired", Retryable: false}
	}
	return status, nil
}

func (c *Codex) CancelDeviceLogin(ctx context.Context, loginID string) error {
	status, err := c.DeviceLoginStatus(loginID)
	if err != nil {
		return err
	}
	if status.State != "pending" {
		return nil
	}
	var response struct{}
	if err := c.request(ctx, "account/login/cancel", map[string]string{"loginId": loginID}, &response); err != nil {
		return err
	}
	c.stateMu.Lock()
	c.logins[loginID] = LoginStatus{LoginID: loginID, State: "cancelled"}
	if c.activeLogin == loginID {
		c.activeLogin = ""
	}
	c.stateMu.Unlock()
	return nil
}

func (c *Codex) Logout(ctx context.Context) error {
	if err := c.Start(ctx); err != nil {
		return err
	}
	return c.request(ctx, "account/logout", map[string]any{}, nil)
}

func (c *Codex) Models(ctx context.Context) ([]CodexModel, error) {
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID          string   `json:"id"`
			DisplayName string   `json:"displayName"`
			Description string   `json:"description"`
			Hidden      bool     `json:"hidden"`
			IsDefault   bool     `json:"isDefault"`
			Modalities  []string `json:"inputModalities"`
		} `json:"data"`
	}
	if err := c.request(ctx, "model/list", map[string]any{"limit": 100, "includeHidden": false}, &response); err != nil {
		return nil, err
	}
	models := make([]CodexModel, 0, len(response.Data))
	for _, model := range response.Data {
		if model.Hidden || model.ID == "" || (len(model.Modalities) != 0 && !containsString(model.Modalities, "text")) {
			continue
		}
		models = append(models, CodexModel{ID: model.ID, DisplayName: model.DisplayName, Description: model.Description, Default: model.IsDefault})
	}
	return models, nil
}

func (c *Codex) noticeLoop() {
	for message := range c.notices {
		if message.Method != "account/login/completed" {
			select {
			case c.turnNotices <- message:
			default:
				c.failProcess(&Error{Code: "assistant.codex_unavailable", Message: "Codex turn notification queue exceeded", Retryable: true})
			}
			continue
		}
		var completed struct {
			LoginID *string `json:"loginId"`
			Success bool    `json:"success"`
			Error   *string `json:"error"`
		}
		if json.Unmarshal(message.Params, &completed) != nil || completed.LoginID == nil {
			continue
		}
		state := "succeeded"
		errorMessage := ""
		if !completed.Success {
			state = "failed"
			if completed.Error != nil {
				errorMessage = strings.TrimSpace(*completed.Error)
			}
		}
		c.stateMu.Lock()
		c.logins[*completed.LoginID] = LoginStatus{LoginID: *completed.LoginID, State: state, Error: errorMessage}
		if c.activeLogin == *completed.LoginID {
			c.activeLogin = ""
		}
		c.stateMu.Unlock()
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
