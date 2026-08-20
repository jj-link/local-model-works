package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
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

func TestSecurityHeadersPermitEmbeddedRouterBootstrap(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := (&Server{}).securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := recorder.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("CSP blocks embedded router bootstrap: %q", csp)
	}
}
