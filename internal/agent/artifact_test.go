package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// TestFetchHFSnapshotRequestsBlobsAndVerifiesBothSiblingKinds proves the
// metadata request carries ?blobs=true (without it Hugging Face omits
// size/lfs on siblings) and that both a regular Git blob and an LFS sibling
// are downloaded and digest-verified using sizes from the metadata.
func TestFetchHFSnapshotRequestsBlobsAndVerifiesBothSiblingKinds(t *testing.T) {
	const (
		owner    = "acme"
		repo     = "tiny-model"
		revision = "deadbeefcafebabe"
		regular  = "config.json" // regular git blob (no LFS)
		lfsFile  = "weights.safetensors"
	)
	regularBody := []byte(`{"hidden_size":42}`)
	lfsBody := []byte(strings.Repeat("lfs", 5000))

	sawBlobs := false
	var resolveCalls atomic.Int32
	var activeResolves atomic.Int32
	var maxActiveResolves atomic.Int32
	var bothResolves sync.Once
	concurrentResolves := make(chan struct{})
	var progress []artifactDownloadProgress
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/api/models/"):
			sawBlobs = request.URL.Query().Get("blobs") == "true"
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(map[string]any{
				"siblings": []map[string]any{
					{"rfilename": regular, "size": int64(len(regularBody))},
					{"rfilename": lfsFile, "lfs": map[string]any{"sha256": hex.EncodeToString(sha256Sum(lfsBody)), "size": int64(len(lfsBody))}},
				},
			})
		case strings.Contains(request.URL.Path, "/resolve/"):
			resolveCalls.Add(1)
			active := activeResolves.Add(1)
			defer activeResolves.Add(-1)
			for {
				maxActive := maxActiveResolves.Load()
				if active <= maxActive || maxActiveResolves.CompareAndSwap(maxActive, active) {
					break
				}
			}
			if active >= 2 {
				bothResolves.Do(func() { close(concurrentResolves) })
			}
			select {
			case <-concurrentResolves:
			case <-time.After(2 * time.Second):
			}
			body := lfsBody
			if strings.HasSuffix(request.URL.Path, regular) {
				body = regularBody
			}
			_, _ = response.Write(body)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	base := mustParseURL(t, server.URL)
	t.Cleanup(func() { hfBaseURL = &url.URL{Scheme: "https", Host: "huggingface.co"} })
	hfBaseURL = base

	cacheRoot := t.TempDir()
	if err := fetchHFSnapshot(t.Context(), "hf://acme/tiny-model@"+revision, cacheRoot, "", func(update artifactDownloadProgress) {
		progress = append(progress, update)
	}); err != nil {
		t.Fatalf("fetchHFSnapshot: %v", err)
	}
	if !sawBlobs {
		t.Fatal("metadata request did not include blobs=true")
	}
	for name := range map[string]string{regular: hex.EncodeToString(sha256Sum(regularBody)), lfsFile: hex.EncodeToString(sha256Sum(lfsBody))} {
		// both must be materialized under the snapshot dir
		if _, err := os.Stat(filepath.Join(cacheRoot, "hub", "models--acme--tiny-model", "snapshots", revision, name)); err != nil {
			t.Errorf("snapshot missing for %s: %v", name, err)
		}
	}
	if len(progress) < 3 || progress[0].Phase != "metadata" || progress[len(progress)-1].Phase != "validating" {
		t.Fatalf("progress phases = %+v", progress)
	}
	final := progress[len(progress)-1]
	wantBytes := uint64(len(regularBody) + len(lfsBody))
	if final.BytesDone != wantBytes || final.BytesTotal != wantBytes || final.FilesDone != 2 || final.FilesTotal != 2 {
		t.Fatalf("final progress = %+v, want %d bytes and 2 files", final, wantBytes)
	}
	if maxActiveResolves.Load() < 2 {
		t.Fatalf("maximum concurrent resolve requests = %d, want at least 2", maxActiveResolves.Load())
	}
	firstResolveCalls := resolveCalls.Load()
	if err := fetchHFSnapshot(t.Context(), "hf://acme/tiny-model@"+revision, cacheRoot, ""); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
	if resolveCalls.Load() != firstResolveCalls {
		t.Fatalf("cached verified snapshot redownloaded %d file(s)", resolveCalls.Load()-firstResolveCalls)
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// TestMakePackageTraversableFixesCachedModes reproduces the spark2/3 failure
// where a recipe package cached by an older agent build left its directories
// at 0700 (staging) / 0750 (assets), so the container's non-agent UID could
// not reach /lmw/assets/serve.sh and reported "Permission denied". The
// helper normalizes the package directory and its subtree to 0755 while
// asset files keep their packed 0555 mode. The parent recipes/ directory is
// chmodded separately by fetchRecipePackage.
func TestMakePackageTraversableFixesCachedModes(t *testing.T) {
	stateRoot := t.TempDir()
	recipes := filepath.Join(stateRoot, "recipes")
	if err := os.MkdirAll(recipes, 0o750); err != nil {
		t.Fatal(err)
	}
	// Simulate a cached package from the old agent build: package dir 0700,
	// assets dir 0750, asset file 0555.
	pkg := filepath.Join(recipes, "cacheddigest")
	assets := filepath.Join(pkg, "assets")
	if err := os.MkdirAll(assets, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "serve.sh"), []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}

	if err := makePackageTraversable(pkg); err != nil {
		t.Fatal(err)
	}

	check := func(dir string) {
		t.Helper()
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Errorf("%s mode = %o, want 0755", dir, mode)
		}
	}
	check(pkg)
	check(assets)

	fileInfo, err := os.Stat(filepath.Join(assets, "serve.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o555 {
		t.Errorf("serve.sh mode = %o, want 0555 (files keep packed mode)", mode)
	}
}

// TestMakeModelTreeReadableFixesCachedModes reproduces the spark2 failure
// where an older agent build fetched a Hugging Face model with 0750 dirs and
// 0640 blobs; the container (root, no CAP_DAC_OVERRIDE) got
// "Permission denied" on config.json. After normalization, every directory
// is 0755, every regular file 0644, and symlinks are left untouched.
func TestMakeModelTreeReadableFixesCachedModes(t *testing.T) {
	cache := t.TempDir()
	modelRoot := filepath.Join(cache, "hub", "models--deepseek-ai--DeepSeek-V4-Flash-0731")
	blobs := filepath.Join(modelRoot, "blobs")
	snap := filepath.Join(modelRoot, "snapshots", "rev123")
	downloads := filepath.Join(modelRoot, ".downloads", "rev123")
	if err := os.MkdirAll(blobs, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snap, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(downloads, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(modelRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	// a 0640 blob (the HF default) + a 0644 blob + a snapshot symlink
	if err := os.WriteFile(filepath.Join(blobs, "blob-a"), []byte("config"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobs, "blob-b"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../blobs/blob-a", filepath.Join(snap, "config.json")); err != nil {
		t.Fatal(err)
	}

	if err := makeModelTreeReadable(modelRoot); err != nil {
		t.Fatal(err)
	}

	wantDir := map[string]bool{modelRoot: true, blobs: true, snap: true, downloads: true}
	for d := range wantDir {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Errorf("%s dir mode = %o, want 0755", d, mode)
		}
	}
	for _, f := range []string{filepath.Join(blobs, "blob-a"), filepath.Join(blobs, "blob-b")} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Errorf("%s file mode = %o, want 0644", f, mode)
		}
	}
	// symlink must still be a symlink pointing at the blob
	linkInfo, err := os.Lstat(filepath.Join(snap, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("config.json is no longer a symlink: %v", linkInfo.Mode())
	}
	if got, err := os.Readlink(filepath.Join(snap, "config.json")); err != nil || got != "../blobs/blob-a" {
		t.Errorf("config.json readlink = %q err=%v, want ../blobs/blob-a", got, err)
	}
}
