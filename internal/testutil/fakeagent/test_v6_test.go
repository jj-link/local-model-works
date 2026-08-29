package fakeagent

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

// transferFixture wires the V6 scenario: a source agent with a valid,
// operator-verified HF-style cache placement, a destination agent that
// advertises a man-in-the-middle relay address, and the artifact row both
// sides report against.
type transferFixture struct {
	t            *testing.T
	s            *Server
	fx           *HFFixture
	src          *Agent
	dst          *Agent
	srcNode      string
	dstNode      string
	art          db.Artifact
	srcBind      string // source's real peer listener (pre-reserved)
	destRel      string // destination-relative transfer root
	total        int64  // transferred byte count of the model repository
	lastTransfer string
}

func completeHFFixture(t *testing.T, fixture *HFFixture, identity string) {
	t.Helper()
	names := []string{
		"config.json",
		"model-00001-of-00002.safetensors",
		"model-00002-of-00002.safetensors",
		"model.safetensors.index.json",
		"extra/pointer.json",
	}
	files := make([]map[string]any, 0, len(names))
	for _, name := range names {
		path := filepath.Join(fixture.Snapshot, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		files = append(files, map[string]any{
			"path": name, "size": len(data), "digest": fmt.Sprintf("sha256:%x", sum),
		})
	}
	manifest, err := json.Marshal(map[string]any{"identity": identity, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fixture.ModelDir, ".lmw", "snapshots", fixture.Sha40+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
}

func bootTransferFixture(t *testing.T, phase1RelayAddr string) *transferFixture {
	t.Helper()
	s := NewServer(t, "", "127.0.0.1:0")
	srcRoot := t.TempDir()
	fx := BuildHFFixture(t, srcRoot, 256<<10)
	srcBind := FreeTCPPort(t)

	tokS, tokD := s.IssueToken(t), s.IssueToken(t)
	src := StartAgent(t, s, AgentOpts{
		Hostname: "spark-src", Token: tokS, IP: "10.0.0.21/24",
		CacheRoots: []string{srcRoot}, PeerBind: srcBind, PeerAdvertise: phase1RelayAddr,
	})
	dst := StartAgent(t, s, AgentOpts{
		Hostname: "spark-dst", Token: tokD, IP: "10.0.0.22/24",
	})
	ns, nd := src.NodeID(), dst.NodeID()
	s.ApproveNode(t, ns)
	s.ApproveNode(t, nd)
	s.WaitOnline(t, ns)
	s.WaitOnline(t, nd)

	// The artifact row (agents report placements by identity, so the
	// identity must match what the agent derives from the cache layout).
	artID, _ := newID()
	art := db.Artifact{ID: artID, Kind: "model", Identity: "hf://acme/test-model@" + fx.Sha40}
	if err := s.Q.CreateArtifact(s.Ctx, db.CreateArtifactParams{
		ID: artID, Kind: art.Kind, Identity: art.Identity,
		Revision: sql.NullString{String: fx.Sha40, Valid: true},
	}); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	storedArtifact, err := s.Q.GetArtifactByIdentity(s.Ctx, art.Identity)
	if err != nil {
		t.Fatalf("get canonical artifact: %v", err)
	}
	art = storedArtifact
	artID = art.ID
	// Operator-verified placement on the source (the agent's own cache scan
	// reports "pending"; this upsert is the controller-side validation).
	completeHFFixture(t, fx, art.Identity)
	var total int64
	for _, file := range WalkTree(t, fx.ModelDir) {
		info, err := os.Lstat(filepath.Join(fx.ModelDir, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			total += file.Size
		}
	}
	if err := s.Q.UpsertPlacement(s.Ctx, db.UpsertPlacementParams{
		ArtifactID: artID, NodeID: ns, Path: fx.ModelDir, State: "valid", SizeBytes: total,
	}); err != nil {
		t.Fatalf("upsert source placement: %v", err)
	}

	return &transferFixture{
		t: t, s: s, fx: fx, src: src, dst: dst, srcNode: ns, dstNode: nd,
		art: art, srcBind: srcBind,
		destRel: "models--acme--test-model",
		total:   total,
	}
}

// dstTree is the destination's transfer root for the fixture's destRel.
func (f *transferFixture) dstTree() string {
	return filepath.Join(f.dst.cfg.TransferDir(), f.destRel)
}

func (f *transferFixture) stagingTree() string {
	return filepath.Join(f.dst.cfg.TransferDir(), ".untrusted-"+f.lastTransfer)
}

// startProxied creates the MITM relay advertised by the source and points it
// at the source's real listener before starting the destination pull.
func (f *transferFixture) startProxied(t *testing.T, proxyAddr string, threshold int64) *Proxy {
	t.Helper()
	dstCert, dstKey := NodeCertPEM(t, f.dst)
	srcCert, srcKey := NodeCertPEM(t, f.src)
	// Incoming client is destination: present source cert; upstream client
	// represents destination to the real source.
	p := NewProxy(t, CAPEM(t, f.s), srcCert, srcKey, dstCert, dstKey, threshold)
	SetDstAddr(f.srcBind)
	p.StartOn(proxyAddr)
	tid, err := f.s.Srv.Deployments().StartTransfer(f.s.Ctx, f.art, f.srcNode, f.dstNode, f.destRel)
	if err != nil {
		t.Fatalf("start transfer: %v", err)
	}
	f.lastTransfer = tid
	t.Logf("transfer %s via relay %s (threshold %d of %d bytes)", tid, proxyAddr, threshold, f.total)
	return p
}

// waitDestValid blocks until the destination's placement is valid (the
// receiver's final report is the success signal).
func (f *transferFixture) waitDestValid(timeout time.Duration) db.ArtifactPlacement {
	f.t.Helper()
	var row db.ArtifactPlacement
	Deadline(f.t, timeout, func() bool {
		rows, err := f.s.Q.ListPlacements(f.s.Ctx, f.art.ID)
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r.NodeID == f.dstNode && r.State == "valid" {
				row = r
				return true
			}
		}
		return false
	}, "destination valid placement")
	return row
}

// TestV6_TransferInterruptResume proves the artifact transfer lifecycle over
// the real mTLS peer protocol: the source streams the HF snapshot through a
// man-in-the-middle relay that pauses mid-transfer; tearing the relay down
// interrupts the stream and the destination discards the partial tree; the
// resume re-dispatches the transfer (new credential, full re-send — the
// designed resume semantics; no offset continuation exists in the wire
// protocol) and completes; the destination tree then matches the source
// file-for-file (resolved digests), passes the product's snapshot
// validator, and only then becomes a valid placement.
func TestV6_TransferInterruptResume(t *testing.T) {
	// Reserve the phase-1 relay's port before the agents boot: the
	// destination advertises it in its inventory, and the source dials
	// exactly that address.
	holdAddr := FreeTCPPort(t)
	f := bootTransferFixture(t, holdAddr)
	s := f.s
	// Gating: the destination holds no valid copy before completion, so it
	// cannot be a transfer source (startTransfer requires a valid placement
	// on the source node, deploy/service.go:1310).
	_, err := s.Srv.Deployments().StartTransfer(s.Ctx, f.art, f.dstNode, f.srcNode, "x")
	if err == nil || !strings.Contains(err.Error(), "holds no valid copy") {
		t.Fatalf("dest-as-source: err = %v, want 'holds no valid copy'", err)
	}

	// Phase 1: start the transfer through a relay that holds the stream
	// mid-transfer (headers + full shard A + part of shard B). The relay
	// binds the address the destination already advertises.
	threshold := int64(1024 + (256 << 10) + (128 << 10))
	p1 := f.startProxied(t, holdAddr, threshold)
	Deadline(t, 20*time.Second, p1.Held, "relay to hold the stream mid-transfer")

	// The partial tree exists: shard A complete.
	dstTree := f.dstTree()
	relativeShardA, _ := filepath.Rel(f.fx.ModelDir, f.fx.BlobFile)
	shardA := filepath.Join(f.stagingTree(), relativeShardA)
	Deadline(t, 20*time.Second, func() bool {
		fi, err := os.Stat(shardA)
		return err == nil && fi.Size() == f.fx.ShardSize
	}, "shard A fully written on the destination")
	// Gating holds mid-transfer: no valid destination placement yet.
	rows, _ := s.Q.ListPlacements(s.Ctx, f.art.ID)
	for _, r := range rows {
		if r.NodeID == f.dstNode && r.State == "valid" {
			t.Fatalf("destination became placement-available before final validation")
		}
	}

	// Interrupt: the untrusted staging tree remains resumable but never
	// becomes a valid placement.
	p1.Close()
	if _, err := os.Stat(f.stagingTree()); err != nil {
		t.Fatalf("untrusted staging was not retained: %v", err)
	}

	// Phase 2: the source advertises a fresh relay and the destination pulls
	// with a fresh one-use credential.
	resumeAddr := FreeTCPPort(t)
	f.src = f.src.Restart(t, f.srcBind, resumeAddr)
	s.WaitPeerListen(t, f.srcNode, resumeAddr)
	p2 := f.startProxied(t, resumeAddr, 0)
	t.Cleanup(p2.Close)

	row := f.waitDestValid(40 * time.Second)

	// Destination repository matches the source, including relative symlinks.
	srcInv := WalkTree(t, f.fx.ModelDir)
	dstInv := WalkTree(t, dstTree)
	CompareTrees(t, "source", srcInv, "destination", dstInv)
	destinationSnapshot := filepath.Join(dstTree, "snapshots", f.fx.Sha40)
	if ds := Validate(destinationSnapshot, dstTree); len(ds) != 0 {
		t.Fatalf("destination snapshot validation: %v", ds)
	}
	// The placement row carries the verified content.
	if row.SizeBytes != f.total {
		t.Errorf("placement size = %d, want %d", row.SizeBytes, f.total)
	}
	if row.Path != dstTree {
		t.Errorf("placement path = %s, want %s", row.Path, dstTree)
	}

	// The transfer ledger recorded both attempts.
	trows, err := s.Q.ListTransfers(s.Ctx)
	if err != nil {
		t.Fatalf("list transfers: %v", err)
	}
	if len(trows) != 2 {
		t.Errorf("transfer rows = %d, want 2 (interrupted + resumed)", len(trows))
	}
}

// TestV6_TransferCorruptionHealedByResend proves the designed corruption
// behavior: a shard corrupted on the destination while a transfer is
// interrupted is healed by the resume, which re-sends every file in full
// (the receiver rewrites each file from the stream — transfer.go os.Create
// — and reports the placement only after the complete re-transfer). The
// interruption is a hard agent stop with the stream still held, so the
// receiver's failure cleanup (RemoveAll) has not yet run and the corrupted
// file genuinely survives until the re-send overwrites it.
func TestV6_TransferCorruptionHealedByResend(t *testing.T) {
	// Reserve the phase-1 relay's port before the agents boot (the
	// destination advertises it; the source dials it).
	holdAddr := FreeTCPPort(t)
	f := bootTransferFixture(t, holdAddr)

	threshold := int64(1024 + (256 << 10) + (128 << 10))
	p1 := f.startProxied(t, holdAddr, threshold)
	Deadline(t, 20*time.Second, p1.Held, "relay to hold the stream mid-transfer")

	dstTree := f.dstTree()
	relativeShardA, _ := filepath.Rel(f.fx.ModelDir, f.fx.BlobFile)
	shardA := filepath.Join(f.stagingTree(), relativeShardA)
	Deadline(t, 20*time.Second, func() bool {
		fi, err := os.Stat(shardA)
		return err == nil && fi.Size() == f.fx.ShardSize
	}, "shard A fully written on the destination")

	// Corrupt the shard on the destination (flip a byte mid-file).
	original, err := os.ReadFile(shardA)
	if err != nil {
		t.Fatalf("read shard A: %v", err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)/2] ^= 0xFF
	if err := os.WriteFile(shardA, corrupt, 0o644); err != nil {
		t.Fatalf("corrupt shard A: %v", err)
	}

	// Hard-stop the destination agent while the stream is still held: the
	// interrupted receiver stays connected, so its RemoveAll cleanup has not
	// run and the corrupted file survives the interruption.
	f.dst.Stop()
	Deadline(t, 15*time.Second, func() bool {
		return f.s.Node(t, f.dstNode).Status == "offline"
	}, "destination agent offline")
	if data, err := os.ReadFile(shardA); err != nil || !bytes.Equal(data, corrupt) {
		t.Fatalf("corrupted shard did not survive the interruption (err %v)", err)
	}

	// Resume with a fresh destination process and a new source relay.
	f.dst = f.dst.Restart(t, "", "")
	resumeAddr := FreeTCPPort(t)
	f.src = f.src.Restart(t, f.srcBind, resumeAddr)
	f.s.WaitPeerListen(t, f.srcNode, resumeAddr)
	p2 := f.startProxied(t, resumeAddr, 0)
	t.Cleanup(p2.Close)
	f.waitDestValid(40 * time.Second)

	// The published tree contains the original, verified shard.
	finalShardA := filepath.Join(dstTree, relativeShardA)
	after, err := os.ReadFile(finalShardA)
	if err != nil {
		t.Fatalf("read shard A after resume: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("corrupted shard survived the resume re-send")
	}
	srcInv := WalkTree(t, f.fx.ModelDir)
	dstInv := WalkTree(t, dstTree)
	CompareTrees(t, "source", srcInv, "destination", dstInv)
	if ds := Validate(filepath.Join(dstTree, "snapshots", f.fx.Sha40), dstTree); len(ds) != 0 {
		t.Fatalf("destination snapshot validation: %v", ds)
	}

	// Teardown: release the first relay; the still-connected interrupted
	// receiver then fails and cleans up (after all assertions).
	p1.Close()
}
