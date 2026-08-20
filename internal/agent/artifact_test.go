package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeHTTPFileContinuesVerifiedLength(t *testing.T) {
	payload := []byte(strings.Repeat("artifact-data-", 1024))
	var gotRange, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		gotRange = request.Header.Get("Range")
		gotAuth = request.Header.Get("Authorization")
		offset := 0
		if gotRange != "" {
			if _, err := fmt.Sscanf(gotRange, "bytes=%d-", &offset); err != nil {
				t.Errorf("range = %q", gotRange)
			}
			response.WriteHeader(http.StatusPartialContent)
		}
		_, _ = response.Write(payload[offset:])
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "artifact.part")
	prefix := len(payload) / 3
	if err := os.WriteFile(destination, payload[:prefix], 0o640); err != nil {
		t.Fatal(err)
	}
	if err := resumeHTTPFile(t.Context(), server.Client(), server.URL, "scoped-token", destination, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if gotRange != fmt.Sprintf("bytes=%d-", prefix) || gotAuth != "Bearer scoped-token" {
		t.Fatalf("headers range=%q auth=%q", gotRange, gotAuth)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("download mismatch: size=%d err=%v", len(got), err)
	}
}

func TestResumeHTTPFileRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("too-large"))
	}))
	defer server.Close()
	if err := resumeHTTPFile(t.Context(), server.Client(), server.URL, "", filepath.Join(t.TempDir(), "part"), 3); err == nil {
		t.Fatal("oversized response accepted")
	}
}
