package agent

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSnapshotDigestNamedSymlinkDoesNotTrustCorruptBytes(t *testing.T) {
	root := t.TempDir()
	expected := []byte("good")
	digest := "sha256:" + hex.EncodeToString(sha256Sum(expected))
	blob := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
	if err := os.WriteFile(blob, []byte("evil"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "weights")
	if err := os.Symlink(filepath.Base(blob), link); err != nil {
		t.Fatal(err)
	}
	if existingSnapshotFile(t.Context(), link, 4, digest) {
		t.Fatal("digest-looking symlink certified corrupt bytes")
	}
}

func TestFetchHFRefetchesCompleteCorruptPartial(t *testing.T) {
	revision := strings.Repeat("a", 40)
	body := []byte(`{"model_type":"test"}`)
	digest := hex.EncodeToString(sha256Sum(body))
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "config.json", "lfs": map[string]any{"size": len(body), "sha256": digest}}}})
			return
		}
		downloads.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	previous := hfBaseURL
	hfBaseURL = mustParseURL(t, server.URL)
	t.Cleanup(func() { hfBaseURL = previous })
	root := t.TempDir()
	partial := filepath.Join(root, "hub", "models--acme--tiny", ".downloads", revision, "config.json.part")
	if err := os.MkdirAll(filepath.Dir(partial), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte(strings.Repeat("x", len(body))), 0644); err != nil {
		t.Fatal(err)
	}
	if err := fetchHFSnapshot(t.Context(), "hf://acme/tiny@"+revision, root, ""); err != nil {
		t.Fatal(err)
	}
	if downloads.Load() != 1 {
		t.Fatalf("corrupt full-length partial was not refetched: %d", downloads.Load())
	}
	snapshot := filepath.Join(root, "hub", "models--acme--tiny", "snapshots", revision, "config.json")
	if !existingSnapshotFile(t.Context(), snapshot, int64(len(body)), "sha256:"+digest) {
		t.Fatal("published snapshot does not match upstream bytes")
	}
}

func TestFetchHFRejectsNonLFSContentWithoutGitObjectMatch(t *testing.T) {
	revision := strings.Repeat("a", 40)
	expected := []byte("good")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/models/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": revision, "siblings": []map[string]any{{"rfilename": "config.json", "size": len(expected), "blobId": gitBlobID(expected)}}})
			return
		}
		_, _ = w.Write([]byte("evil"))
	}))
	defer server.Close()
	previous := hfBaseURL
	hfBaseURL = mustParseURL(t, server.URL)
	t.Cleanup(func() { hfBaseURL = previous })
	root := t.TempDir()
	if err := fetchHFSnapshot(t.Context(), "hf://acme/tiny@"+revision, root, ""); err == nil {
		t.Fatal("same-size bytes with the wrong Git blob object ID accepted")
	}
	if _, err := os.Stat(hfSnapshotManifestPath(filepath.Join(root, "hub", "models--acme--tiny"), revision)); !os.IsNotExist(err) {
		t.Fatal("failed integrity check published a completion record")
	}
}
