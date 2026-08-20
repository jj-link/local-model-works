package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/config"
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
