package agent

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/config"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestPeerResumeRevalidatesRecordedPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "weights.part")
	good := []byte("verified-prefix")
	sum := hex.EncodeToString(sha256Sum(good))
	checkpoint := &peerCheckpoint{Prefixes: map[string]transferPrefix{"weights": {Offset: uint64(len(good)), SHA256: "sha256:" + sum}}}
	if err := os.WriteFile(path, append(append([]byte(nil), good...), []byte("uncommitted")...), 0600); err != nil {
		t.Fatal(err)
	}
	file, _, offset, err := checkpoint.openPrefix(t.Context(), path, "weights", 100, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if offset != uint64(len(good)) {
		t.Fatalf("verified prefix was discarded: offset=%d", offset)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(good) {
		t.Fatal("uncommitted suffix was retained")
	}
	corrupt := append([]byte(nil), good...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	file, _, offset, err = checkpoint.openPrefix(t.Context(), path, "weights", 100, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if offset != 0 {
		t.Fatal("corrupt prefix was trusted during resume")
	}
}

func TestOwnedPeerCheckpointSurvivesAgentRestartCleanup(t *testing.T) {
	root := t.TempDir()
	a := &Agent{cfg: config.Agent{StateRoot: root}}
	command := &agentv1.TransferCommand{TransferId: "attempt", ArtifactIdentity: "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DestPath: "artifact"}
	credential := &transferCred{SourceDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	staging, _, err := a.transferCheckpoint(command, credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(staging, 0755); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(staging, "prefix")
	if err := os.WriteFile(owned, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupTransferStaging(a.cfg.TransferDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); err != nil {
		t.Fatalf("restart deleted an owned interrupted prefix: %v", err)
	}
}

func TestDestinationWriterLockCoversNestedLegacyAndNewPaths(t *testing.T) {
	root := t.TempDir()
	a := &Agent{cfg: config.Agent{StateRoot: root}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, at, _, err := a.beginAcquisition(ctx, "legacy", "hf://acme/model@revision", filepath.Join(root, "cache"), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.beginAcquisition(ctx, "download", "hf://acme/model@revision", filepath.Join(root, "cache", "snapshots", "revision"), "download"); err == nil {
		t.Fatal("nested command obtained a second writer")
	}
	a.finishAcquisition(at, nil, context.Canceled)
	if _, next, fresh, err := a.beginAcquisition(ctx, "resume", "hf://acme/model@revision", filepath.Join(root, "cache"), "resume"); err != nil || !fresh {
		t.Fatalf("quiescent destination stayed locked: %v", err)
	} else {
		a.finishAcquisition(next, nil, nil)
	}
}
