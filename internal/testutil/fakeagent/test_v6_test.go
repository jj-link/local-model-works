package fakeagent

import (
	"bytes"
	"database/sql"
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
	t       *testing.T
	s       *Server
	fx      *HFFixture
	src     *Agent
	dst     *Agent
	srcNode string
	dstNode string
	art     db.Artifact
	dstBind string // destination's real peer listener (pre-reserved)
	destRel string // destination-relative transfer root
	total   int64  // resolved byte count of the snapshot
}

func bootTransferFixture(t *testing.T, phase1RelayAddr string) *transferFixture {
	t.Helper()
	s := NewServer(t, "", "127.0.0.1:0")
	srcRoot := t.TempDir()
	fx := BuildHFFixture(t, srcRoot, 256<<10)
	dstBind := FreeTCPPort(t)

	tokS, tokD := s.IssueToken(t), s.IssueToken(t)
	src := StartAgent(t, s, AgentOpts{
		Hostname: "spark-src", Token: tokS, IP: "10.0.0.21/24",
		CacheRoots: []string{srcRoot},
	})
	// The destination advertises the phase-1 relay address (bound later by
	// the test's proxy); its real listener is the pre-reserved dstBind.
	dst := StartAgent(t, s, AgentOpts{
		Hostname: "spark-dst", Token: tokD, IP: "10.0.0.22/24",
		PeerBind: dstBind, PeerAdvertise: phase1RelayAddr,
	})
	ns, nd := src.NodeID(), dst.NodeID()
	s.ApproveNode(t, ns)
	s.ApproveNode(t, nd)
	s.WaitOnline(t, ns)
	s.WaitOnline(t, nd)

	// The artifact row (agents report placements by identity, so the
	// identity must match what the agent derives from the cache layout).
	artID, _ := newID()
	art := db.Artifact{ID: artID, Kind: "huggingface", Identity: "huggingface://acme/test-model"}
	if err := s.Q.CreateArtifact(s.Ctx, db.CreateArtifactParams{
		ID: artID, Kind: art.Kind, Identity: art.Identity,
		Revision: sql.NullString{String: fx.Sha40, Valid: true},
	}); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	// Operator-verified placement on the source (the agent's own cache scan
	// reports "pending"; this upsert is the controller-side validation).
	var total int64
	for _, f := range WalkTree(t, fx.Snapshot) {
		total += f.Size
	}
	if err := s.Q.UpsertPlacement(s.Ctx, db.UpsertPlacementParams{
		ArtifactID: artID, NodeID: ns, Path: fx.Snapshot, State: "valid", SizeBytes: total,
	}); err != nil {
		t.Fatalf("upsert source placement: %v", err)
	}

	return &transferFixture{
		t: t, s: s, fx: fx, src: src, dst: dst, srcNode: ns, dstNode: nd,
		art: art, dstBind: dstBind,
		destRel: "models--acme--test-model/snapshots/" + fx.Sha40,
		total:   total,
	}
}

// dstTree is the destination's transfer root for the fixture's destRel.
func (f *transferFixture) dstTree() string {
	return filepath.Join(f.dst.cfg.TransferDir(), f.destRel)
}

// startProxied creates the MITM relay on proxyAddr (already advertised by
// the destination's inventory), points it at the destination's real
// listener, and starts the real StartTransfer source->dest.
func (f *transferFixture) startProxied(t *testing.T, proxyAddr string, threshold int64) *Proxy {
	t.Helper()
	dstCert, dstKey := NodeCertPEM(t, f.dst)
	srcCert, srcKey := NodeCertPEM(t, f.src)
	p := NewProxy(t, CAPEM(t, f.s), dstCert, dstKey, srcCert, srcKey, threshold)
	SetDstAddr(f.dstBind)
	p.StartOn(proxyAddr)
	tid, err := f.s.Srv.Deployments().StartTransfer(f.s.Ctx, f.art, f.srcNode, f.dstNode, f.destRel)
	if err != nil {
		t.Fatalf("start transfer: %v", err)
	}
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
	shardA := filepath.Join(dstTree, filepath.Base(f.fx.ShardA))
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

	// Interrupt: tear the relay down. The receiver's stream fails and it
	// removes the partial tree (transfer.go fail -> RemoveAll); the source
	// acks the error to the controller.
	p1.Close()
	Deadline(t, 20*time.Second, func() bool {
		_, err := os.Stat(dstTree)
		return os.IsNotExist(err)
	}, "partial destination tree removed after interrupt")

	// Phase 2: resume. The destination re-advertises a fresh relay address
	// (agent restart, same identity) and a new StartTransfer re-dispatches
	// the copy — a full re-send is the designed resume.
	resumeAddr := FreeTCPPort(t)
	f.dst = f.dst.Restart(t, f.dstBind, resumeAddr)
	// The source dials the address stored in the destination's inventory
	// row; wait for the re-advertisement to persist before dispatching.
	s.WaitPeerListen(t, f.dstNode, resumeAddr)
	p2 := f.startProxied(t, resumeAddr, 0)
	t.Cleanup(p2.Close)

	row := f.waitDestValid(40 * time.Second)

	// Destination tree matches the source file-for-file (symlinks are
	// dereferenced on the wire, so the destination materializes them as
	// regular files with identical content).
	srcInv := WalkTree(t, f.fx.Snapshot)
	dstInv := WalkTree(t, dstTree)
	CompareTrees(t, "source", srcInv, "destination", dstInv)
	// The destination snapshot passes the product's validator.
	if ds := Validate(dstTree, filepath.Dir(filepath.Dir(dstTree))); len(ds) != 0 {
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
	shardA := filepath.Join(dstTree, filepath.Base(f.fx.ShardA))
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

	// Resume: restart the destination (fresh relay address, same identity),
	// new StartTransfer — a full re-send overwrites the corrupted shard.
	resumeAddr := FreeTCPPort(t)
	f.dst = f.dst.Restart(t, f.dstBind, resumeAddr)
	// Wait for the re-advertisement to persist in the inventory row
	// before dispatching the re-send.
	f.s.WaitPeerListen(t, f.dstNode, resumeAddr)
	p2 := f.startProxied(t, resumeAddr, 0)
	t.Cleanup(p2.Close)
	f.waitDestValid(40 * time.Second)

	// The corrupted byte is gone: the re-send rewrote the file.
	after, err := os.ReadFile(shardA)
	if err != nil {
		t.Fatalf("read shard A after resume: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("corrupted shard survived the resume re-send")
	}
	// Full tree agreement and validator pass.
	srcInv := WalkTree(t, f.fx.Snapshot)
	dstInv := WalkTree(t, dstTree)
	CompareTrees(t, "source", srcInv, "destination", dstInv)
	if ds := Validate(dstTree, filepath.Dir(filepath.Dir(dstTree))); len(ds) != 0 {
		t.Fatalf("destination snapshot validation: %v", ds)
	}

	// Teardown: release the first relay; the still-connected interrupted
	// receiver then fails and cleans up (after all assertions).
	p1.Close()
}
