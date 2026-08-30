package agent

import (
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
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jj-link/local-model-works/internal/ca"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	agentv1connect "github.com/jj-link/local-model-works/proto/agent/v1/agentv1connect"
)

const (
	transferChunkSize = 64 << 10
	transferRootFile  = ".lmw-root-file"
)

type transferCred struct {
	TransferID   string `json:"transfer_id"`
	RunID        string `json:"run_id"`
	SourceNode   string `json:"source_node"`
	DestNode     string `json:"dest_node"`
	ArtifactID   string `json:"artifact_id"`
	SrcPath      string `json:"src_path"`
	SourceDigest string `json:"source_digest"`
	SrcSize      int64  `json:"src_size"`
	DestPath     string `json:"dest_path"`
	ExpUnix      int64  `json:"exp_unix"`
	Signature    string `json:"signature"`
}

func (c *transferCred) canonicalJSON() []byte {
	copy := *c
	copy.Signature = ""
	data, _ := json.Marshal(&copy)
	return data
}

type transferSession struct {
	credential string
	files      map[string]*agentv1.FileEntry
	finished   map[string]bool
}

type transferService struct {
	a      *Agent
	ln     net.Listener
	server *http.Server
	cancel context.CancelFunc
	mu     sync.Mutex
	active map[string]*transferSession
	used   map[string]bool
}

func (a *Agent) verifyCredential(credential *transferCred) error {
	if credential.TransferID == "" || credential.SourceNode == "" || credential.DestNode == "" ||
		credential.ArtifactID == "" || credential.SrcPath == "" || credential.DestPath == "" {
		return fmt.Errorf("credential scope is incomplete")
	}
	if time.Now().Unix() > credential.ExpUnix {
		return fmt.Errorf("credential expired at %d", credential.ExpUnix)
	}
	a.mu.Lock()
	caPEM, caPub := a.caPEM, a.caPub
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
	signature, err := base64.StdEncoding.DecodeString(credential.Signature)
	if err != nil {
		return fmt.Errorf("credential signature: %w", err)
	}
	if err := ca.VerifyECDSA(caPub, credential.canonicalJSON(), signature); err != nil {
		return fmt.Errorf("credential signature: %w", err)
	}
	return nil
}

func (a *Agent) newTransferService(ctx context.Context) (*transferService, error) {
	if err := os.MkdirAll(a.cfg.TransferDir(), 0o750); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", a.cfg.PeerAddr)
	if err != nil {
		return nil, fmt.Errorf("peer transfer listener: %w", err)
	}
	nodeCert := a.certSnapshot()
	if nodeCert == nil {
		listener.Close()
		return nil, fmt.Errorf("node certificate not loaded")
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			certificate := a.certSnapshot()
			if certificate == nil {
				return nil, fmt.Errorf("node certificate not loaded")
			}
			return certificate, nil
		},
		ClientCAs: a.caPool(), ClientAuth: tls.RequireAndVerifyClientCert,
		NextProtos: []string{"h2"},
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	service := &transferService{
		a: a, ln: listener, cancel: cancel,
		active: map[string]*transferSession{}, used: map[string]bool{},
	}
	pattern, handler := agentv1connect.NewPeerTransferServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle(pattern, peerCertificateMiddleware(handler))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		listener.Close()
		cancel()
		return nil, err
	}
	service.server = server
	bound := listener.Addr().String()
	a.cfg.PeerAddr = bound
	if a.cfg.PeerAdvertise == "" {
		a.cfg.PeerAdvertise = bound
	}
	go func() {
		if err := server.Serve(tls.NewListener(listener, tlsConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent: peer transfer server: %v", err)
		}
	}()
	go func() {
		<-serviceCtx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	return service, nil
}

func peerCertificateMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			http.Error(response, "client certificate required", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(request.Context(), peerCertContextKey{}, request.TLS.PeerCertificates[0])
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

type peerCertContextKey struct{}

func (t *transferService) stop() {
	t.cancel()
	_ = t.server.Close()
	_ = t.ln.Close()
}

func decodeTransferCredential(encoded string) (*transferCred, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var credential transferCred
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, err
	}
	return &credential, nil
}

func (t *transferService) authorize(ctx context.Context, transferID, encoded string) (*transferCred, error) {
	credential, err := decodeTransferCredential(encoded)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	if err := t.a.verifyCredential(credential); err != nil {
		return nil, err
	}
	if credential.TransferID != transferID || credential.SourceNode != t.a.NodeID() {
		return nil, fmt.Errorf("credential source or transfer mismatch")
	}
	peer, _ := ctx.Value(peerCertContextKey{}).(*x509.Certificate)
	if peer == nil || !contains(peer.DNSNames, credential.DestNode) {
		return nil, fmt.Errorf("credential destination not in peer certificate")
	}
	return credential, nil
}

func (t *transferService) Manifest(ctx context.Context, request *connect.Request[agentv1.ManifestRequest]) (*connect.Response[agentv1.ManifestResponse], error) {
	credential, err := t.authorize(ctx, request.Msg.GetTransferId(), request.Msg.GetCredential())
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	if filepath.Clean(request.Msg.GetPath()) != filepath.Clean(credential.SrcPath) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("source path mismatch"))
	}
	entries, total, treeDigest, err := collectManifest(ctx, credential.SrcPath, hfContainDir(credential.SrcPath))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if credential.SrcSize > 0 && int64(total) != credential.SrcSize {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source size changed"))
	}
	if credential.SourceDigest != "" && credential.SourceDigest != treeDigest {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("source digest changed"))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used[credential.TransferID] {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("credential already used"))
	}
	if existing := t.active[credential.TransferID]; existing != nil && existing.credential != credential.Signature {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("credential replay mismatch"))
	}
	files := make(map[string]*agentv1.FileEntry, len(entries))
	for _, entry := range entries {
		files[entry.Path] = entry
	}
	t.active[credential.TransferID] = &transferSession{
		credential: credential.Signature, files: files, finished: map[string]bool{},
	}
	return connect.NewResponse(&agentv1.ManifestResponse{Files: entries, TotalBytes: total, TreeDigest: treeDigest}), nil
}

func (t *transferService) ReadChunk(ctx context.Context, request *connect.Request[agentv1.ReadChunkRequest]) (*connect.Response[agentv1.ReadChunkResponse], error) {
	credential, err := t.authorize(ctx, request.Msg.GetTransferId(), request.Msg.GetCredential())
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	t.mu.Lock()
	session := t.active[credential.TransferID]
	var entry *agentv1.FileEntry
	if session != nil && session.credential == credential.Signature {
		entry = session.files[request.Msg.GetPath()]
	}
	t.mu.Unlock()
	if entry == nil || entry.GetSymlinkTarget() != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is not in transfer manifest"))
	}
	length := request.Msg.GetLength()
	if length == 0 || length > transferChunkSize {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("chunk length out of bounds"))
	}
	if request.Msg.GetOffset() > entry.GetSize() {
		return nil, connect.NewError(connect.CodeOutOfRange, fmt.Errorf("offset past EOF"))
	}
	path := credential.SrcPath
	if entry.GetPath() != transferRootFile {
		path = filepath.Join(credential.SrcPath, filepath.FromSlash(entry.GetPath()))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	defer file.Close()
	if _, err := file.Seek(int64(request.Msg.GetOffset()), io.SeekStart); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	remaining := entry.GetSize() - request.Msg.GetOffset()
	if uint64(length) > remaining {
		length = uint32(remaining)
	}
	data := make([]byte, length)
	read, err := io.ReadFull(file, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	data = data[:read]
	eof := request.Msg.GetOffset()+uint64(read) == entry.GetSize()
	if eof {
		t.mu.Lock()
		session.finished[entry.GetPath()] = true
		if len(session.finished) == regularFileCount(session.files) {
			t.used[credential.TransferID] = true
			delete(t.active, credential.TransferID)
		}
		t.mu.Unlock()
	}
	return connect.NewResponse(&agentv1.ReadChunkResponse{Data: data, Offset: request.Msg.GetOffset(), Eof: eof}), nil
}

func regularFileCount(files map[string]*agentv1.FileEntry) int {
	count := 0
	for _, entry := range files {
		if entry.GetSymlinkTarget() == "" {
			count++
		}
	}
	return count
}

func collectManifest(ctx context.Context, root, contain string) ([]*agentv1.FileEntry, uint64, string, error) {
	root = filepath.Clean(root)
	if !pathWithin(root, contain) {
		return nil, 0, "", fmt.Errorf("source escapes containment root")
	}
	contain = filepath.Clean(contain)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, 0, "", err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, 0, "", fmt.Errorf("source root must not be a symlink")
	}
	if rootInfo.Mode().IsRegular() {
		file, err := os.Open(root)
		if err != nil {
			return nil, 0, "", err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		file.Close()
		if copyErr != nil {
			return nil, 0, "", copyErr
		}
		entry := &agentv1.FileEntry{Path: transferRootFile, Size: uint64(size), Mode: uint32(rootInfo.Mode().Perm()), Sha256: "sha256:" + hex.EncodeToString(hash.Sum(nil))}
		tree := sha256.New()
		fmt.Fprintf(tree, "%s\x00%d\x00%s\x00\n", entry.Path, entry.Size, entry.Sha256)
		return []*agentv1.FileEntry{entry}, uint64(size), "sha256:" + hex.EncodeToString(tree.Sum(nil)), nil
	}
	var entries []*agentv1.FileEntry
	var total uint64
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if current == root {
			return nil
		}
		rel, err := filepath.Rel(root, current)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("path escapes source")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil || filepath.IsAbs(target) {
				return fmt.Errorf("unsafe symlink %s", rel)
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil || !pathWithin(resolved, contain) {
				return fmt.Errorf("symlink %s escapes source", rel)
			}
			entries = append(entries, &agentv1.FileEntry{Path: filepath.ToSlash(rel), Mode: uint32(os.ModeSymlink), SymlinkTarget: filepath.ToSlash(target)})
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		hash := sha256.New()
		read, err := io.Copy(hash, file)
		file.Close()
		if err != nil {
			return err
		}
		total += uint64(read)
		entries = append(entries, &agentv1.FileEntry{
			Path: filepath.ToSlash(rel), Size: uint64(read), Mode: uint32(info.Mode().Perm()),
			Sha256: "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		})
		return nil
	})
	if err != nil {
		return nil, 0, "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	hash := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(hash, "%s\x00%d\x00%s\x00%s\n", entry.Path, entry.Size, entry.Sha256, entry.SymlinkTarget)
	}
	return entries, total, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func pathWithin(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

func makeTransferredArtifactMountable(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.Chmod(path, info.Mode().Perm()|0o055)
		case info.Mode().IsRegular():
			return os.Chmod(path, info.Mode().Perm()|0o044)
		default:
			return nil
		}
	})
}

func (a *Agent) handleTransfer(ctx context.Context, command *agentv1.TransferCommand) {
	if command.GetRole() != "dest" {
		return
	}
	if err := a.pullTransfer(ctx, command); err != nil {
		a.transferError(command.GetTransferId(), err.Error())
	}
}

func (a *Agent) pullTransfer(ctx context.Context, command *agentv1.TransferCommand) error {
	timeout := command.GetTimeoutSeconds()
	if timeout == 0 {
		timeout = 900
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	credential, err := decodeTransferCredential(command.GetCredential())
	if err != nil {
		return err
	}
	if credential.TransferID != command.GetTransferId() || credential.DestNode != a.NodeID() ||
		credential.ArtifactID != command.GetArtifactIdentity() || credential.DestPath != command.GetDestPath() {
		return fmt.Errorf("transfer command does not match credential")
	}
	nodeCert := a.certSnapshot()
	if nodeCert == nil {
		return fmt.Errorf("node certificate not loaded")
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*nodeCert},
		RootCAs: a.caPool(), InsecureSkipVerify: true, VerifyPeerCertificate: a.peerVerifyChain,
		NextProtos: []string{"h2"},
	}
	httpClient := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsConfig}}
	client := agentv1connect.NewPeerTransferServiceClient(httpClient, "https://"+command.GetPeerAddress())
	manifestResponse, err := client.Manifest(ctx, connect.NewRequest(&agentv1.ManifestRequest{
		TransferId: command.GetTransferId(), Credential: command.GetCredential(), Path: credential.SrcPath,
	}))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	staging := filepath.Join(a.cfg.TransferDir(), ".untrusted-"+command.GetTransferId())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	var done uint64
	for _, entry := range manifestResponse.Msg.GetFiles() {
		rel, err := safeRelativePath(entry.GetPath())
		if err != nil {
			return err
		}
		destination := filepath.Join(staging, rel)
		if entry.GetSymlinkTarget() != "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		offset := uint64(0)
		if info, err := os.Stat(destination); err == nil && uint64(info.Size()) == entry.GetSize() {
			if matchesFileDigest(destination, entry.GetSha256()) {
				offset = entry.GetSize()
			} else if err := os.Remove(destination); err != nil {
				return err
			}
		} else if err == nil {
			if err := os.Remove(destination); err != nil {
				return err
			}
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.FileMode(entry.GetMode())&0o777)
		if err != nil {
			return err
		}
		for offset < entry.GetSize() {
			response, err := client.ReadChunk(ctx, connect.NewRequest(&agentv1.ReadChunkRequest{
				TransferId: command.GetTransferId(), Credential: command.GetCredential(), Path: entry.GetPath(),
				Offset: offset, Length: transferChunkSize,
			}))
			if err != nil {
				file.Close()
				return fmt.Errorf("read %s: %w", entry.GetPath(), err)
			}
			if response.Msg.GetOffset() != offset || len(response.Msg.GetData()) == 0 {
				file.Close()
				return fmt.Errorf("invalid chunk for %s", entry.GetPath())
			}
			if _, err := file.Write(response.Msg.GetData()); err != nil {
				file.Close()
				return err
			}
			offset += uint64(len(response.Msg.GetData()))
			done += uint64(len(response.Msg.GetData()))
			a.sendTransferProgress(command.GetTransferId(), done, manifestResponse.Msg.GetTotalBytes())
		}
		if err := file.Close(); err != nil {
			return err
		}
		if !matchesFileDigest(destination, entry.GetSha256()) {
			return fmt.Errorf("digest mismatch for %s", entry.GetPath())
		}
	}
	for _, entry := range manifestResponse.Msg.GetFiles() {
		if entry.GetSymlinkTarget() == "" {
			continue
		}
		rel, err := safeRelativePath(entry.GetPath())
		if err != nil {
			return err
		}
		target := filepath.Clean(filepath.FromSlash(entry.GetSymlinkTarget()))
		link := filepath.Join(staging, rel)
		if filepath.IsAbs(target) || !pathWithin(filepath.Join(filepath.Dir(link), target), staging) {
			return fmt.Errorf("unsafe symlink target for %s", entry.GetPath())
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(filepath.ToSlash(target), link); err != nil && !os.IsExist(err) {
			return err
		}
	}
	_, _, treeDigest, err := collectManifest(ctx, staging, staging)
	if err != nil || treeDigest != manifestResponse.Msg.GetTreeDigest() {
		return fmt.Errorf("final tree digest mismatch")
	}
	destinationRel, err := safeRelativePath(command.GetDestPath())
	if err != nil {
		return err
	}
	final := filepath.Join(a.cfg.TransferDir(), destinationRel)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	rootFile := len(manifestResponse.Msg.GetFiles()) == 1 && manifestResponse.Msg.GetFiles()[0].GetPath() == transferRootFile
	if rootFile {
		if err := os.Rename(filepath.Join(staging, transferRootFile), final); err != nil {
			return err
		}
		if err := os.Remove(staging); err != nil {
			return err
		}
	} else if err := os.Rename(staging, final); err != nil {
		return err
	}
	if err := makeTransferredArtifactMountable(final); err != nil {
		return fmt.Errorf("make transferred artifact mountable: %w", err)
	}
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_PlacementReport{
		PlacementReport: &agentv1.PlacementReport{
			ArtifactId: credential.ArtifactID, Path: final, State: "valid",
			VerifiedAt: timestamppb.Now(), SizeBytes: manifestResponse.Msg.GetTotalBytes(),
		},
	}})
	a.sendTransferProgress(command.GetTransferId(), manifestResponse.Msg.GetTotalBytes(), manifestResponse.Msg.GetTotalBytes())
	return nil
}

func cleanupTransferStaging(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".untrusted-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func safeRelativePath(value string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(value))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe relative path %q", value)
	}
	return clean, nil
}

func matchesFileDigest(path, want string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return "sha256:"+hex.EncodeToString(hash.Sum(nil)) == want
}

func (a *Agent) sendTransferProgress(transferID string, done, total uint64) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_TransferProgress{
		TransferProgress: &agentv1.TransferProgress{TransferId: transferID, BytesDone: done, BytesTotal: total},
	}})
}

func (a *Agent) transferError(transferID, message string) {
	log.Printf("agent: transfer %s: %s", transferID, message)
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Ack{
		Ack: &agentv1.Ack{CommandId: transferID, Ok: false, Error: message},
	}})
}

func hfContainDir(source string) string {
	clean := filepath.Clean(source)
	if filepath.Base(filepath.Dir(clean)) == "snapshots" {
		model := filepath.Dir(filepath.Dir(clean))
		if strings.HasPrefix(filepath.Base(model), "models--") {
			return model
		}
	}
	return clean
}

func (a *Agent) peerVerifyChain(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("peer sent no certificate")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: a.caPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return err
	}
	if len(leaf.DNSNames) == 0 {
		return errors.New("peer certificate has no node ID SAN")
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
