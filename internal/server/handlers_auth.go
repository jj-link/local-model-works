package server

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
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
