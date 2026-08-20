package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestWebServeReturnsAssetInsteadOfSPAFallback(t *testing.T) {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		t.Fatalf("web dist: %v", err)
	}
	assets, err := fs.ReadDir(sub, "assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("embedded assets: %v (%d entries)", err, len(assets))
	}
	name := assets[0].Name()
	recorder := httptest.NewRecorder()
	webServe(recorder, httptest.NewRequest(http.MethodGet, "/assets/"+name, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("asset status: %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("asset returned SPA HTML: %q", contentType)
	}
}

func TestWebServeReturns404ForMissingAsset(t *testing.T) {
	recorder := httptest.NewRecorder()
	webServe(recorder, httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing asset status: %d", recorder.Code)
	}
	if strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("missing asset returned SPA HTML")
	}
}

func TestWebServeFallsBackToSPAIndex(t *testing.T) {
	recorder := httptest.NewRecorder()
	webServe(recorder, httptest.NewRequest(http.MethodGet, "/serving/deployments", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("SPA status: %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("SPA content type: %q", contentType)
	}
}

func TestSecurityHeadersNonceEveryEmbeddedScript(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := (&Server{}).securityHeaders(http.HandlerFunc(webServe))
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := recorder.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "'unsafe-inline'") || !strings.Contains(csp, "script-src 'self' 'nonce-") {
		t.Fatalf("CSP = %q", csp)
	}
	matches := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
	if len(matches) != 2 {
		t.Fatalf("CSP nonce = %q", csp)
	}
	if !strings.Contains(recorder.Body.String(), `nonce="`+matches[1]+`"`) {
		t.Fatalf("embedded bootstrap lacks CSP nonce")
	}
}
