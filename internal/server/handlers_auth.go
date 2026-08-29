package server

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
)

// dummyHash burns one argon2 verification on unknown usernames so login
// timing does not reveal which usernames exist.
var dummyHash = func() string {
	h, _ := auth.HashPassword("timing-equalizer")
	return h
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type browserLoginRequest struct {
	Token string `json:"token"`
}

func sessionView(s *auth.Session) map[string]any {
	return map[string]any{
		"username":   s.Username,
		"csrf_token": s.CSRF,
		"expires_at": s.Expires.Format(time.RFC3339),
	}
}

func (s *Server) requireConfiguredOrigin(w http.ResponseWriter, r *http.Request) bool {
	expectedOrigin, err := s.cfg.NormalizedPublicOrigin()
	if err != nil || r.Header.Get("Origin") != expectedOrigin {
		writeErr(w, http.StatusForbidden, "auth.origin", "missing or invalid Origin")
		return false
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireConfiguredOrigin(w, r) {
		return
	}

	var req loginRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "auth.invalid_body", err.Error())
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErr(w, http.StatusUnprocessableEntity, "auth.invalid_body", "username and password are required")
		return
	}
	user, err := s.q.GetUser(r.Context(), req.Username)
	switch {
	case err == nil:
		if !auth.VerifyPassword(req.Password, user.Argon2Hash) {
			writeErr(w, http.StatusUnauthorized, "auth.bad_credentials", "invalid username or password")
			return
		}
	case errors.Is(err, sql.ErrNoRows):
		auth.VerifyPassword(req.Password, dummyHash())
		writeErr(w, http.StatusUnauthorized, "auth.bad_credentials", "invalid username or password")
		return
	default:
		handleErr(w, err)
		return
	}
	sess, err := s.sessions.Login(req.Username)
	if err != nil {
		handleErr(w, err)
		return
	}
	http.SetCookie(w, sessionCookieFor(sess.Token, int(s.cfg.SessionTTL.Seconds())))
	writeJSON(w, http.StatusOK, sessionView(sess))
}

func (s *Server) handleBrowserLogin(w http.ResponseWriter, r *http.Request) {
	if !s.requireConfiguredOrigin(w, r) {
		return
	}
	var req browserLoginRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "auth.invalid_body", err.Error())
		return
	}
	raw, err := hex.DecodeString(req.Token)
	if err != nil || len(raw) != 32 {
		writeErr(w, http.StatusUnauthorized, "auth.bad_browser_token", "invalid or expired browser login token")
		return
	}
	tokenHash := auth.SHA256([]byte(req.Token))
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		handleErr(w, err)
		return
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	username, err := qtx.ConsumeBrowserLoginToken(r.Context(), db.ConsumeBrowserLoginTokenParams{
		TokenHash: hex.EncodeToString(tokenHash[:]),
		ExpiresAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusUnauthorized, "auth.bad_browser_token", "invalid or expired browser login token")
		return
	}
	if err != nil {
		handleErr(w, err)
		return
	}
	if _, err := qtx.AppendEvent(r.Context(), db.AppendEventParams{
		Type: "auth.browser_login", Payload: string(mustJSON(map[string]any{
			"username": username, "method": "one_time_token",
		})),
	}); err != nil {
		handleErr(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		handleErr(w, err)
		return
	}
	sess, err := s.sessions.Login(username)
	if err != nil {
		handleErr(w, err)
		return
	}
	http.SetCookie(w, sessionCookieFor(sess.Token, int(s.cfg.SessionTTL.Seconds())))
	writeJSON(w, http.StatusOK, sessionView(sess))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(sessionCookie)
	if cookie != nil {
		s.sessions.Logout(cookie.Value)
	}
	http.SetCookie(w, sessionCookieFor("", -1))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "auth.unauthorized", "missing or invalid session")
		return
	}
	sess, err := s.sessions.Validate(cookie.Value, "", false)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "auth.unauthorized", "missing or invalid session")
		return
	}
	writeJSON(w, http.StatusOK, sessionView(sess))
}

func sessionCookieFor(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}
