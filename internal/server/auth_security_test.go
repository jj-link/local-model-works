package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/db"
)

type sessionRecord struct {
	username  string
	csrfHash  string
	expiresAt string
}

func newTestSessions() *auth.Sessions {
	records := map[string]sessionRecord{}
	return &auth.Sessions{
		TTL: time.Hour,
		Create: func(tokenHash, username, csrfHash, expiresAt string) error {
			records[tokenHash] = sessionRecord{username: username, csrfHash: csrfHash, expiresAt: expiresAt}
			return nil
		},
		Get: func(tokenHash string) (string, string, string, error) {
			record, ok := records[tokenHash]
			if !ok {
				return "", "", "", http.ErrNoCookie
			}
			return record.username, record.csrfHash, record.expiresAt, nil
		},
		Delete: func(tokenHash string) error {
			delete(records, tokenHash)
			return nil
		},
	}
}

func TestSessionCookieIsStrictAndSecure(t *testing.T) {
	cookie := sessionCookieFor("opaque", 3600)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags = HttpOnly:%t Secure:%t SameSite:%d", cookie.HttpOnly, cookie.Secure, cookie.SameSite)
	}
}

func TestAuthenticatedMutationRequiresConfiguredOriginAndCSRF(t *testing.T) {
	sessions := newTestSessions()
	session, err := sessions.Login("operator")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	server := &Server{
		cfg:      config.Server{PublicOrigin: "https://lmw.example.test"},
		sessions: sessions,
	}
	called := false
	handler := server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		origin     string
		csrf       string
		wantStatus int
		wantCode   string
	}{
		{name: "browser origin and CSRF", origin: "https://lmw.example.test", csrf: session.CSRF, wantStatus: http.StatusNoContent},
		{name: "missing origin", csrf: session.CSRF, wantStatus: http.StatusForbidden, wantCode: "auth.origin"},
		{name: "wrong origin", origin: "https://attacker.example.test", csrf: session.CSRF, wantStatus: http.StatusForbidden, wantCode: "auth.origin"},
		{name: "missing CSRF", origin: "https://lmw.example.test", wantStatus: http.StatusForbidden, wantCode: "auth.csrf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/example", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			if test.csrf != "" {
				req.Header.Set("X-CSRF-Token", test.csrf)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if test.wantCode != "" && !strings.Contains(recorder.Body.String(), test.wantCode) {
				t.Fatalf("response = %s, want code %s", recorder.Body.String(), test.wantCode)
			}
			if got := recorder.Code == http.StatusNoContent; called != got {
				t.Fatalf("handler called = %t, want %t", called, got)
			}
		})
	}
}

func TestLoginRequiresConfiguredOrigin(t *testing.T) {
	server := &Server{cfg: config.Server{PublicOrigin: "https://lmw.example.test"}}
	for _, origin := range []string{"", "https://attacker.example.test"} {
		request := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/login", nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		server.handleLogin(recorder, request)
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "auth.origin") {
			t.Fatalf("origin %q: status=%d body=%s", origin, recorder.Code, recorder.Body.String())
		}
	}
}

func TestBrowserLoginTokenIsOriginBoundOneUseAndCreatesNormalSession(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	hash, err := auth.HashPassword("unused-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateUser(ctx, db.CreateUserParams{Username: "operator", Argon2Hash: hash}); err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tokenHash := fmt.Sprintf("%x", auth.SHA256([]byte(token)))
	if err := queries.CreateBrowserLoginToken(ctx, db.CreateBrowserLoginTokenParams{
		TokenHash: tokenHash, Username: "operator",
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	sessions := newTestSessions()
	server := &Server{
		cfg: config.Server{PublicOrigin: "https://lmw.example.test", SessionTTL: time.Hour},
		db:  database, q: queries, sessions: sessions,
	}
	body, _ := json.Marshal(browserLoginRequest{Token: token})
	wrongOrigin := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/browser-login", bytes.NewReader(body))
	wrongOrigin.Header.Set("Content-Type", "application/json")
	wrongOrigin.Header.Set("Origin", "https://attacker.example.test")
	wrongRecorder := httptest.NewRecorder()
	server.handleBrowserLogin(wrongRecorder, wrongOrigin)
	if wrongRecorder.Code != http.StatusForbidden {
		t.Fatalf("wrong origin status = %d: %s", wrongRecorder.Code, wrongRecorder.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/browser-login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://lmw.example.test")
	recorder := httptest.NewRecorder()
	server.handleBrowserLogin(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("browser login status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var sessionBody struct {
		Username string `json:"username"`
		CSRF     string `json:"csrf_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if sessionBody.Username != "operator" || sessionBody.CSRF == "" {
		t.Fatalf("session = %+v", sessionBody)
	}
	var sessionCookieValue string
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookie {
			sessionCookieValue = cookie.Value
		}
	}
	if sessionCookieValue == "" {
		t.Fatal("session cookie not issued")
	}
	called := false
	protected := server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	protectedRequest := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/example", nil)
	protectedRequest.Header.Set("Origin", "https://lmw.example.test")
	protectedRequest.Header.Set("X-CSRF-Token", sessionBody.CSRF)
	protectedRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionCookieValue})
	protectedRecorder := httptest.NewRecorder()
	protected.ServeHTTP(protectedRecorder, protectedRequest)
	if protectedRecorder.Code != http.StatusNoContent || !called {
		t.Fatalf("authenticated mutation status = %d: %s", protectedRecorder.Code, protectedRecorder.Body.String())
	}

	replay := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/browser-login", bytes.NewReader(body))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Origin", "https://lmw.example.test")
	replayRecorder := httptest.NewRecorder()
	server.handleBrowserLogin(replayRecorder, replay)
	if replayRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d: %s", replayRecorder.Code, replayRecorder.Body.String())
	}
	var auditCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'auth.browser_login'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("browser login audit events = %d", auditCount)
	}
}

func TestBrowserLoginTokenRejectsExpiry(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "auth-expired.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	hash, _ := auth.HashPassword("unused-password")
	if err := queries.CreateUser(ctx, db.CreateUserParams{Username: "operator", Argon2Hash: hash}); err != nil {
		t.Fatal(err)
	}
	token, _ := auth.NewToken()
	now := time.Now().UTC()
	if err := queries.CreateBrowserLoginToken(ctx, db.CreateBrowserLoginTokenParams{
		TokenHash: fmt.Sprintf("%x", auth.SHA256([]byte(token))), Username: "operator",
		CreatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: config.Server{PublicOrigin: "https://lmw.example.test", SessionTTL: time.Hour},
		db:  database, q: queries, sessions: newTestSessions(),
	}
	body, _ := json.Marshal(browserLoginRequest{Token: token})
	request := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/browser-login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://lmw.example.test")
	recorder := httptest.NewRecorder()
	server.handleBrowserLogin(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestServiceBearerUsesOnlyExplicitScopesAndRejectsMixedCredentials(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "api-token.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	token, err := auth.NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	queries := db.New(database)
	if err := queries.CreateAPIToken(ctx, db.CreateAPITokenParams{
		ID: "01900000-0000-7000-8000-000000000070", Name: "factory",
		TokenHash: auth.APITokenHash(token), ScopesJson: `["deployments:read","benchmarks:write"]`,
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{q: queries, sessions: newTestSessions()}
	called := false
	protected := server.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		principal := auth.APITokenPrincipalFromContext(r.Context())
		if principal == nil || principal.ID != "01900000-0000-7000-8000-000000000070" || principal.Name != "factory" {
			t.Fatalf("principal = %+v", principal)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		cookie     bool
		wantStatus int
	}{
		{name: "deployment read", method: http.MethodGet, path: "/api/v1/deployments", token: token, wantStatus: http.StatusNoContent},
		{name: "benchmark create", method: http.MethodPost, path: "/api/v1/benchmarks", token: token, wantStatus: http.StatusNoContent},
		{name: "missing benchmark read", method: http.MethodGet, path: "/api/v1/benchmarks/catalog", token: token, wantStatus: http.StatusForbidden},
		{name: "arbitrary endpoint", method: http.MethodGet, path: "/api/v1/nodes", token: token, wantStatus: http.StatusForbidden},
		{name: "malformed bearer", method: http.MethodGet, path: "/api/v1/deployments", token: "not-a-token", wantStatus: http.StatusUnauthorized},
		{name: "mixed cookie", method: http.MethodGet, path: "/api/v1/deployments", token: token, cookie: true, wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called = false
			request := httptest.NewRequest(test.method, "https://lmw.example.test"+test.path, nil)
			request.Header.Set("Authorization", "Bearer "+test.token)
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session"})
			}
			recorder := httptest.NewRecorder()
			protected.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if called != (test.wantStatus == http.StatusNoContent) {
				t.Fatalf("handler called = %t", called)
			}
		})
	}
	if _, err := database.Exec(`UPDATE api_tokens SET revoked_at='2025-01-01T00:00:00Z' WHERE name='factory'`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://lmw.example.test/api/v1/deployments", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	protected.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
