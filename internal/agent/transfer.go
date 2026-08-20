package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/ca"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// transferService is the agent's peer-to-peer artifact transfer listener.
// Wire protocol (newline-delimited JSON, then bytes):
//
//	cred     JSON object (peer credential, CA-signed)
//	manifest JSON object (file list with sha256/size per path)
//	bytes    concatenated file contents, paths from the manifest
//	ack      JSON object {sha256, size_bytes, complete, dest_path}
//
// The receiver verifies the credential against the persisted CA, then
// writes the tree under its transfer root (root-validated relative paths
// only).
type transferService struct {
	a      *Agent
	ln     net.Listener
	cancel context.CancelFunc
}

// transferCred is the peer credential carried in-band per the protocol.
// The controller's CA signs the canonical form (Signature blanked) so
// receivers can trust the sender's node identity and scope.
type transferCred struct {
	Role         string `json:"role"` // "source" or "dest"
	NodeID       string `json:"node_id"`
	ArtifactID   string `json:"artifact_id"`
	SrcPath      string `json:"src_path"` // absolute path on the source (file or tree)
	ExpUnix      int64  `json:"exp_unix"`
	SourceSha256 string `json:"source_sha256"`
	SrcSize      int64  `json:"src_size"`
	DestSha256   string `json:"dest_sha256"`
	PeerAddr     string `json:"peer_addr"`
	DestPath     string `json:"dest_path"` // relative write root under the dest transfer dir
	// Signature is the CA's ASN.1 ECDSA signature over canonicalJSON()
	// (base64, Signature itself blanked).
	Signature string `json:"signature"`
}

// canonicalJSON is the byte form the CA signs and the receiver verifies.
func (c *transferCred) canonicalJSON() []byte {
	cp := *c
	cp.Signature = ""
	data, _ := json.Marshal(&cp)
	return data
}

// verifyCredential validates a peer credential: expiry, then the CA
// signature over the canonical form.
func (a *Agent) verifyCredential(cred *transferCred) error {
	if time.Now().Unix() > cred.ExpUnix {
		return fmt.Errorf("credential expired at %d", cred.ExpUnix)
	}
	a.mu.Lock()
	caPEM := a.caPEM
	caPub := a.caPub
	a.mu.Unlock()
	if len(caPEM) == 0 {
		return fmt.Errorf("no CA persisted yet")
	}
	if caPub == nil {
		var ok bool
		caPub, ok = caPubFromPEM(caPEM)
		if !ok {
			return fmt.Errorf("CA public key not parseable")
		}
	}
	sig, err := base64.StdEncoding.DecodeString(cred.Signature)
	if err != nil {
		return fmt.Errorf("credential signature: %w", err)
	}
	if err := ca.VerifyECDSA(caPub, cred.canonicalJSON(), sig); err != nil {
		return fmt.Errorf("credential signature: %w", err)
	}
	return nil
}

// newTransferService starts the peer listener. It is called once at agent
// startup and survives control sessions.
func (a *Agent) newTransferService(ctx context.Context) (*transferService, error) {
	dir := a.cfg.TransferDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", a.cfg.PeerAddr)
	if err != nil {
		return nil, fmt.Errorf("peer transfer listener: %w", err)
	}
	svc := &transferService{a: a, ln: ln}
	_, cancel := context.WithCancel(ctx)
	svc.cancel = cancel
	go svc.serve(ctx)
	return svc, nil
}

func (t *transferService) serve(ctx context.Context) {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.a.handlePeerConnection(ctx, conn)
	}
}

func (t *transferService) stop() {
	t.ln.Close()
	t.cancel()
}

// handlePeerConnection runs one receiver-side transfer.
func (a *Agent) handlePeerConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	nodeCert := a.certSnapshot()
	if nodeCert == nil {
		return
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*nodeCert},
		// Go's TLS server verifies client certificates against ClientCAs
		// (RootCAs is server-cert trust only); the CA signed the peer's
		// node certificate at enrollment.
		ClientCAs:  a.caPool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	tc := tls.Server(conn, tlsCfg)
	defer tc.Close()
	if err := tc.HandshakeContext(ctx); err != nil {
		return
	}
	br := bufio.NewReader(tc)
	cred, err := readJSONLine(br)
	if err != nil {
		return
	}
	if err := a.verifyCredential(cred); err != nil {
		writeJSON(tc, map[string]string{"error": err.Error()})
		return
	}
	// The TLS client cert is already CA-verified; additionally the
	// credential's sender node ID must appear in the peer's SANs.
	if st := tc.ConnectionState(); len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		if !contains(leaf.DNSNames, cred.NodeID) {
			writeJSON(tc, map[string]string{"error": "credential node not in peer certificate"})
			return
		}
	}
	var manifest struct {
		SrcPath string     `json:"src_path"`
		Size    int64      `json:"size"`
		Files   []wireFile `json:"files"`
	}
	if _, err := readJSONLine(br, &manifest); err != nil {
		writeJSON(tc, map[string]string{"error": "manifest: " + err.Error()})
		return
	}
	if filepath.Clean(manifest.SrcPath) != filepath.Clean(cred.SrcPath) || len(manifest.Files) == 0 {
		writeJSON(tc, map[string]string{"error": "manifest/cred mismatch"})
		return
	}
	if manifest.Size < 0 || (cred.SrcSize > 0 && manifest.Size != cred.SrcSize) {
		writeJSON(tc, map[string]string{"error": "manifest/cred size mismatch"})
		return
	}
	rel := strings.TrimSpace(cred.DestPath)
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		writeJSON(tc, map[string]string{"error": "invalid dest_path"})
		return
	}
	root := filepath.Join(a.cfg.TransferDir(), filepath.FromSlash(rel))
	fail := func(err string) {
		os.RemoveAll(root)
		writeJSON(tc, map[string]string{"error": err})
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fail(err.Error())
		return
	}
	h := sha256.New()
	var size int64
	for _, wf := range manifest.Files {
		relp := filepath.FromSlash(wf.Path)
		if relp == "" || strings.HasPrefix(relp, "/") || strings.Contains(relp, "..") {
			fail("invalid file path")
			return
		}
		fpath := filepath.Join(root, relp)
		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			fail(err.Error())
			return
		}
		f, err := os.Create(fpath)
		if err != nil {
			fail(err.Error())
			return
		}
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(br, wf.Size))
		f.Close()
		if err != nil {
			fail("stream: " + err.Error())
			return
		}
		if n != wf.Size {
			fail(fmt.Sprintf("short read on %s: %d of %d", relp, n, wf.Size))
			return
		}
		size += n
	}
	if size != manifest.Size {
		fail(fmt.Sprintf("stream size mismatch: %d of %d", size, manifest.Size))
		return
	}
	writeJSON(tc, map[string]any{
		"sha256":     hex.EncodeToString(h.Sum(nil)),
		"size_bytes": size,
		"complete":   true,
		"dest_path":  filepath.ToSlash(rel),
	})
	// The receiver is the placement authority for what it just wrote.
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_PlacementReport{
		PlacementReport: &agentv1.PlacementReport{
			ArtifactId: cred.ArtifactID,
			Path:       root,
			State:      "valid",
			SizeBytes:  uint64(size),
			VerifiedAt: timestamppb.Now(),
		},
	}})
}

// readJSONLine reads one newline-terminated JSON object. With no target it
// returns the generic credential; with a target it unmarshals into it.
func readJSONLine(br *bufio.Reader, v ...any) (*transferCred, error) {
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		var c transferCred
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, err
		}
		return &c, nil
	}
	return nil, json.Unmarshal(line, v[0])
}

// wireFile is one regular-file entry in the transfer manifest. Symlinks
// are dereferenced on the source, so every entry is plain file content.
type wireFile struct {
	Path string `json:"path"` // relative to the write root
	Size int64  `json:"size"`
}

func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// collectFiles walks root (dereferencing symlinks) and returns the regular
// files with paths relative to root, plus their total size. Every resolved
// path must stay inside contain — HF snapshots symlink into a sibling
// blobs/ directory, so contain is the enclosing model directory.
func collectFiles(ctx context.Context, root, contain string) ([]wireFile, int64, error) {
	contain = filepath.Clean(contain)
	var files []wireFile
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if ctx.Err() != nil {
			return filepath.SkipDir
		}
		target := p
		if d.Type()&fs.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil {
				return nil // dangling symlink: not part of the payload
			}
			target = resolved
		}
		fi, serr := os.Stat(target)
		if serr != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rl := filepath.Clean(target)
		if rl != contain && !strings.HasPrefix(rl, contain+string(filepath.Separator)) {
			return fmt.Errorf("symlink %s escapes %s", p, contain)
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		files = append(files, wireFile{Path: filepath.ToSlash(rel), Size: fi.Size()})
		total += fi.Size()
		return nil
	})
	return files, total, err
}

// hfContainDir widens a HF snapshot path to its enclosing models-- directory
// so blobs/ symlinks stay inside the containment root.
func hfContainDir(src string) string {
	p := filepath.Clean(src)
	if filepath.Base(filepath.Dir(p)) == "snapshots" {
		model := filepath.Dir(filepath.Dir(p))
		if strings.HasPrefix(filepath.Base(model), "models--") {
			return model
		}
	}
	return p
}

// handleTransfer executes one TransferCommand. In the "source" role this
// agent streams the file to the peer's listener; in the "dest" role the
// local listener does the work and the command is accepted here.
func (a *Agent) handleTransfer(ctx context.Context, tc *agentv1.TransferCommand) {
	if tc.GetRole() != "source" {
		return
	}
	timeout := tc.GetTimeoutSeconds()
	if timeout == 0 {
		timeout = 900
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	credBytes, err := base64.StdEncoding.DecodeString(tc.GetCredential())
	if err != nil {
		a.transferError(tc.GetTransferId(), "credential: "+err.Error())
		return
	}
	var cred transferCred
	if err := json.Unmarshal(credBytes, &cred); err != nil {
		a.transferError(tc.GetTransferId(), "credential: "+err.Error())
		return
	}
	src := filepath.Clean(cred.SrcPath)
	if !filepath.IsAbs(src) {
		src = filepath.Join(a.cfg.TransferDir(), filepath.FromSlash(src))
	}
	var files []wireFile
	var srcPaths []string // absolute source path per manifest entry
	var total int64
	fi, err := os.Stat(src)
	if err != nil {
		a.transferError(tc.GetTransferId(), "source: "+err.Error())
		return
	}
	switch {
	case fi.IsDir():
		files, total, err = collectFiles(ctx, src, hfContainDir(src))
		if err != nil {
			a.transferError(tc.GetTransferId(), "source: "+err.Error())
			return
		}
		if len(files) == 0 {
			a.transferError(tc.GetTransferId(), "source: no files under "+src)
			return
		}
		for _, wf := range files {
			srcPaths = append(srcPaths, filepath.Join(src, filepath.FromSlash(wf.Path)))
		}
	case fi.Mode().IsRegular():
		files = []wireFile{{Path: filepath.ToSlash(filepath.Base(src)), Size: fi.Size()}}
		srcPaths = []string{src}
		total = fi.Size()
	default:
		a.transferError(tc.GetTransferId(), "source: not a file or directory")
		return
	}
	nodeCert := a.certSnapshot()
	if nodeCert == nil {
		a.transferError(tc.GetTransferId(), "node certificate not loaded")
		return
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{*nodeCert},
		// The peer's leaf is an enrolled node cert (node-ID SANs, not the
		// dialed address), so the chain and node identity are checked
		// explicitly instead of by hostname.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: a.peerVerifyChain,
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", tc.GetPeerAddress())
	if err != nil {
		a.transferError(tc.GetTransferId(), "dial: "+err.Error())
		return
	}
	conn := tls.Client(raw, tlsCfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		a.transferError(tc.GetTransferId(), "handshake: "+err.Error())
		return
	}

	bw := bufio.NewWriter(conn)
	if err := writeJSON(bw, cred); err != nil {
		a.transferError(tc.GetTransferId(), "cred: "+err.Error())
		return
	}
	manifest := map[string]any{
		"src_path": src,
		"size":     total,
		"files":    files,
	}
	if err := writeJSON(bw, manifest); err != nil {
		a.transferError(tc.GetTransferId(), "manifest: "+err.Error())
		return
	}
	h := sha256.New()
	for i, wf := range files {
		f, oerr := os.Open(srcPaths[i])
		if oerr != nil {
			a.transferError(tc.GetTransferId(), "open: "+oerr.Error())
			return
		}
		if _, cerr := io.Copy(io.MultiWriter(bw, h), io.LimitReader(f, wf.Size)); cerr != nil {
			f.Close()
			a.transferError(tc.GetTransferId(), "stream: "+cerr.Error())
			return
		}
		f.Close()
	}
	if ferr := bw.Flush(); ferr != nil {
		a.transferError(tc.GetTransferId(), "flush: "+ferr.Error())
		return
	}
	br := bufio.NewReader(conn)
	var ack map[string]any
	if _, err := readJSONLine(br, &ack); err != nil {
		a.transferError(tc.GetTransferId(), "ack: "+err.Error())
		return
	}
	if e, _ := ack["error"].(string); e != "" {
		a.transferError(tc.GetTransferId(), "peer: "+e)
		return
	}
	if got, _ := ack["sha256"].(string); got != hex.EncodeToString(h.Sum(nil)) {
		a.transferError(tc.GetTransferId(), "peer sha mismatch")
		return
	}
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_TransferProgress{
		TransferProgress: &agentv1.TransferProgress{
			TransferId: tc.GetTransferId(),
			BytesDone:  uint64(total),
			BytesTotal: uint64(total),
		},
	}})
}

// transferError reports a failed transfer to the controller: the Ack now
// carries the failure status, and the server routes it to the transfers row.
func (a *Agent) transferError(transferID, msg string) {
	log.Printf("agent: transfer %s: %s", transferID, msg)
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Ack{
		Ack: &agentv1.Ack{CommandId: transferID, Ok: false, Error: msg},
	}})
}

// peerVerifyChain validates a peer node certificate: chain to the
// controller CA plus a node-ID SAN (enrolled agents only).
func (a *Agent) peerVerifyChain(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("peer sent no certificate")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     a.caPool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return err
	}
	if len(leaf.DNSNames) == 0 {
		return errors.New("peer certificate has no node ID SAN")
	}
	return nil
}

// contains reports whether s is in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
