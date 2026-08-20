package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// webFS embeds the built frontend. The build copies web/dist into
// internal/server/web/dist before compiling; a placeholder index keeps
// plain `go build ./...` working on a fresh clone.
//
//go:embed all:web/dist
var webFS embed.FS

type cspNonceKey struct{}

var scriptOpenTag = regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>`)

func newCSPNonce() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(value[:]), nil
}

func cspPolicy(nonce string) string {
	return "default-src 'self'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self'; img-src 'self' data:; connect-src 'self'"
}

func contextWithCSPNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, cspNonceKey{}, nonce)
}

func cspNonceFrom(ctx context.Context) string {
	nonce, _ := ctx.Value(cspNonceKey{}).(string)
	return nonce
}

func webIndex(w http.ResponseWriter, r *http.Request) {
	nonce := cspNonceFrom(r.Context())
	if nonce == "" {
		var err error
		nonce, err = newCSPNonce()
		if err != nil {
			http.Error(w, "secure random unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Security-Policy", cspPolicy(nonce))
	}
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	page := scriptOpenTag.ReplaceAllStringFunc(string(data), func(tag string) string {
		if strings.Contains(strings.ToLower(tag), " nonce=") {
			return tag
		}
		return strings.TrimSuffix(tag, ">") + ` nonce="` + nonce + `">`
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func webServe(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
		return
	}
	webIndex(w, r)
}
