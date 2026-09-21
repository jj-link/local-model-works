package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestUnchangedWeightVerificationDoesNotReadContentsAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights")
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, _, err := digestFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_ACCESS); err != nil {
		t.Fatal(err)
	}
	if !existingSnapshotFile(t.Context(), path, 4, digest) {
		t.Fatal("unchanged verified weights rejected")
	}
	if _, _, _, err := collectManifest(t.Context(), path, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	var events [4096]byte
	if n, err := syscall.Read(fd, events[:]); n > 0 || !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("unchanged weights were read again: bytes=%d error=%v", n, err)
	}
}

func TestVerifiedWeightsRejectSameSizeCorruptionWithRestoredMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights")
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, _, err := digestFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("evil"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if existingSnapshotFile(t.Context(), path, 4, digest) {
		t.Fatal("same-size corruption with restored mtime accepted")
	}
	if err := os.WriteFile(path, []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	if !existingSnapshotFile(t.Context(), path, 4, digest) {
		t.Fatal("repaired weights did not pass full verification")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if existingSnapshotFile(t.Context(), path, 4, digest) {
		t.Fatal("deleted weights accepted from cached verification")
	}
}

func TestVerifiedWeightSymlinkReplacementIsRechecked(t *testing.T) {
	root := t.TempDir()
	good, evil, link := filepath.Join(root, "good"), filepath.Join(root, "evil"), filepath.Join(root, "weights")
	for path, content := range map[string]string{good: "good", evil: "evil"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	digest, _, err := digestFile(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	if existingSnapshotFile(t.Context(), link, 4, digest) {
		t.Fatal("replacement symlink reused the old target's verification")
	}
}
