package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBlobDigestValidation(t *testing.T) {
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("payload")
	sum := sha256.Sum256(payload)
	valid := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(blobDir, valid[7:]), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	other := []byte("other")
	oSum := sha256.Sum256(other)
	otherValid := "sha256:" + hex.EncodeToString(oSum[:])

	cases := []struct {
		name    string
		d       ociDescriptor
		wantErr bool
	}{
		{"valid", ociDescriptor{Digest: valid, Size: int64(len(payload))}, false},
		{"empty digest", ociDescriptor{}, true},
		{"short digest", ociDescriptor{Digest: "sha256:abc"}, true},
		{"uppercase hex", ociDescriptor{Digest: "sha256:" + strings.Repeat("A", 64)}, true},
		{"traversal suffix", ociDescriptor{Digest: "sha256:../etc/passwd"}, true},
		{"wrong algorithm", ociDescriptor{Digest: "md5:" + strings.Repeat("a", 32)}, true},
		{"well-formed but wrong blob", ociDescriptor{Digest: otherValid, Size: int64(len(other))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := readBlob(dir, tc.d)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for digest %q", tc.d.Digest)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestReadBlobNoPanicOnShortDigest(t *testing.T) {
	dir := t.TempDir()
	// Must return an error, not panic, on a digest shorter than the prefix.
	err := readBlob(dir, ociDescriptor{Digest: "sha256:"})
	if err == nil {
		t.Fatal("expected an error for a truncated digest")
	}
}

func TestLoadAssetPathSafety(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(ok, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := loadAsset(dir, "ok.txt"); err != nil || string(data) != "x" {
		t.Fatalf("loadAsset(ok.txt) = %q, %v; want x, nil", data, err)
	}

	for _, bad := range []string{"", "/", "/abs", "..", "a/../b", "a/./b", "a//b", "a/.."} {
		if _, err := loadAsset(dir, bad); err == nil {
			t.Fatalf("loadAsset(%q) expected an error", bad)
		}
	}

	// Final-entry symlink is rejected.
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(ok, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadAsset(dir, "link.txt"); err == nil {
		t.Fatal("final-entry symlink expected an error")
	}

	// Intermediate symlink escaping the root is rejected: linkdir -> t.TempDir
	// parent holding an outside file.
	outside := t.TempDir()
	outFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkdir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(outside, linkdir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadAsset(dir, "linkdir/secret.txt"); err == nil {
		t.Fatal("intermediate symlink escaping the root expected an error")
	}

	// Missing file is a clean error.
	if _, err := loadAsset(dir, "nope.txt"); err == nil {
		t.Fatal("missing asset expected an error")
	}
}

func TestPackAlwaysHasOneAssetLayerAndAssetsChangeIdentity(t *testing.T) {
	doc := []byte(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe"}`)
	empty, err := PackManifest(doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.LayerDigest == "" || empty.LayerSize == 0 {
		t.Fatalf("empty package layer = %q size=%d", empty.LayerDigest, empty.LayerSize)
	}
	first, err := PackManifest(doc, map[string][]byte{"serve.sh": []byte("echo one\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PackManifest(doc, map[string][]byte{"serve.sh": []byte("echo two\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConfigDigest != second.ConfigDigest || first.ManifestDigest == second.ManifestDigest {
		t.Fatalf("asset change identity: config %q/%q manifest %q/%q",
			first.ConfigDigest, second.ConfigDigest, first.ManifestDigest, second.ManifestDigest)
	}
}

func TestPersistPackageWritesCompleteSourceIndependentLayout(t *testing.T) {
	res, err := PackManifest([]byte(`{"kind":"Recipe"}`), map[string][]byte{"bin/serve.sh": []byte("#!/bin/sh\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "recipes")
	t.Cleanup(func() { _ = RemovePackage(root) })
	dir, created, err := PersistPackage(root, res)
	if err != nil || !created {
		t.Fatalf("persist = %q created=%t err=%v", dir, created, err)
	}
	if err := VerifyLayout(dir); err != nil {
		t.Fatalf("stored layout: %v", err)
	}
	asset, err := os.ReadFile(filepath.Join(dir, "assets", "bin", "serve.sh"))
	if err != nil || string(asset) != "#!/bin/sh\n" {
		t.Fatalf("stored asset = %q, err=%v", asset, err)
	}
	if _, created, err := PersistPackage(root, res); err != nil || created {
		t.Fatalf("idempotent persist created=%t err=%v", created, err)
	}
}

func TestPackEnforcesAssetLimitsBeforeWriting(t *testing.T) {
	tooMany := make(map[string][]byte, MaxAssetFiles+1)
	for i := 0; i <= MaxAssetFiles; i++ {
		tooMany[fmt.Sprintf("%03d.txt", i)] = []byte("x")
	}
	if _, err := PackManifest([]byte(`{}`), tooMany, nil); err == nil {
		t.Fatal("too many assets accepted")
	}
	if _, err := PackManifest([]byte(`{}`), map[string][]byte{
		"large.bin": make([]byte, MaxAssetFileBytes+1),
	}, nil); err == nil {
		t.Fatal("oversized asset accepted")
	}
}
