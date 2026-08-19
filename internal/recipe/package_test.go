package recipe

import (
	"crypto/sha256"
	"encoding/hex"
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
