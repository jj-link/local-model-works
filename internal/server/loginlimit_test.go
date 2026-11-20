package server

import (
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

// newLoginLimitServer returns a Server with user "operator" / "right-password"
// and a controllable limiter clock.
func newLoginLimitServer(t *testing.T) (*Server, *time.Time) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "login-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.New(database)
	hash, err := auth.HashPassword("right-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateUser(ctx, db.CreateUserParams{Username: "operator", Argon2Hash: hash}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	server := &Server{
		cfg: config.Server{PublicOrigin: "https://lmw.example.test", SessionTTL: time.Hour},
		db:  database, q: queries, sessions: newTestSessions(),
	}
	server.logins.now = func() time.Time { return now }
	return server, &now
}

func postLogin(t *testing.T, server *Server, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	request := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://lmw.example.test")
	recorder := httptest.NewRecorder()
	server.handleLogin(recorder, request)
	return recorder
}

func assertTooManyAttempts(t *testing.T, recorder *httptest.ResponseRecorder, label string) {
	t.Helper()
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("%s: status = %d, want 429: %s", label, recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "900" {
		t.Fatalf("%s: Retry-After = %q, want 900", label, got)
	}
	var body Error
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "auth.too_many_attempts" {
		t.Fatalf("%s: code = %q", label, body.Code)
	}
}

func TestLoginLocksUsernameAfterFiveFailuresUntilWindowResets(t *testing.T) {
	server, now := newLoginLimitServer(t)
	for i := 1; i <= 5; i++ {
		if rec := postLogin(t, server, "operator", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad password %d: status = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	assertTooManyAttempts(t, postLogin(t, server, "operator", "wrong"), "sixth bad password")
	assertTooManyAttempts(t, postLogin(t, server, "operator", "right-password"), "correct password while locked")
	assertTooManyAttempts(t, postLogin(t, server, "OPERATOR", "right-password"), "differently cased username")

	// Unknown usernames count too, and lock independently of "operator".
	for i := 1; i <= 5; i++ {
		if rec := postLogin(t, server, "ghost", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown user attempt %d: status = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	assertTooManyAttempts(t, postLogin(t, server, "ghost", "wrong"), "sixth unknown-user attempt")

	*now = now.Add(15*time.Minute - time.Second)
	assertTooManyAttempts(t, postLogin(t, server, "operator", "right-password"), "just before window reset")
	*now = now.Add(time.Second)
	if rec := postLogin(t, server, "operator", "right-password"); rec.Code != http.StatusOK {
		t.Fatalf("login after window reset: status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSuccessfulLoginClearsUsernameFailures(t *testing.T) {
	server, _ := newLoginLimitServer(t)
	for i := 1; i <= 4; i++ {
		if rec := postLogin(t, server, "operator", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad password %d: status = %d", i, rec.Code)
		}
	}
	if rec := postLogin(t, server, "operator", "right-password"); rec.Code != http.StatusOK {
		t.Fatalf("good login: status = %d: %s", rec.Code, rec.Body.String())
	}
	// Without the reset this would be the ninth failure and locked.
	for i := 1; i <= 5; i++ {
		if rec := postLogin(t, server, "operator", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad password %d after success: status = %d", i, rec.Code)
		}
	}
	assertTooManyAttempts(t, postLogin(t, server, "operator", "wrong"), "sixth bad password after success")
}

func TestGlobalLoginBackstopLocksEveryoneForOneMinute(t *testing.T) {
	server, now := newLoginLimitServer(t)
	// Spray: one failure each across 100 usernames, none individually locked.
	for i := 0; i < 100; i++ {
		server.logins.fail(fmt.Sprintf("spray-%d", i))
	}
	assertTooManyAttempts(t, postLogin(t, server, "operator", "right-password"), "login during global lock")

	request := httptest.NewRequest(http.MethodPost, "https://lmw.example.test/api/v1/browser-login", strings.NewReader(`{"token":"00"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://lmw.example.test")
	recorder := httptest.NewRecorder()
	server.handleBrowserLogin(recorder, request)
	assertTooManyAttempts(t, recorder, "browser login during global lock")

	*now = now.Add(time.Minute)
	if rec := postLogin(t, server, "operator", "right-password"); rec.Code != http.StatusOK {
		t.Fatalf("login after global window: status = %d: %s", rec.Code, rec.Body.String())
	}
}
