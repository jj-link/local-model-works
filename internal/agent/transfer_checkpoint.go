package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/recipe"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type transferPrefix struct {
	Offset uint64 `json:"offset"`
	SHA256 string `json:"sha256"`
}
type peerCheckpoint struct {
	Identity    string                    `json:"identity"`
	Destination string                    `json:"destination"`
	TreeDigest  string                    `json:"tree_digest"`
	Prefixes    map[string]transferPrefix `json:"prefixes"`
	path        string
}

func (a *Agent) transferDestination(command *agentv1.TransferCommand) (string, error) {
	if command.GetOp() == agentv1.TransferOp_TRANSFER_OP_UNSPECIFIED {
		rel, err := safeRelativePath(command.GetDestPath())
		if err != nil {
			return "", err
		}
		destination := filepath.Join(a.cfg.TransferDir(), rel)
		return destination, safeDestination(destination)
	}
	identity := command.GetArtifactIdentity()
	parsed, err := artifactidentity.Parse(identity)
	if err != nil {
		return "", err
	}
	spec := downloads.ResourceSpec{Kind: downloads.ResourceArtifact, Identity: identity, Destination: command.GetDestPath()}
	switch parsed.Kind {
	case "model":
		repository, revision, _ := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
		spec.Source = downloads.SourceSpec{Type: downloads.SourceHuggingFace, Reference: repository, Revision: revision}
	case "recipe":
		spec.Kind = downloads.ResourceRecipe
		spec.Source = downloads.SourceSpec{Type: downloads.SourceRecipe, Digest: parsed.Digest}
	case "file":
		spec.Source = downloads.SourceSpec{Type: downloads.SourceLocal, Digest: parsed.Digest}
	default:
		return "", fmt.Errorf("download.transfer_layout_unsupported")
	}
	_, err = a.authorizeDownload(context.Background(), spec)
	return spec.Destination, err
}
func (a *Agent) authorizePeerSource(credential *transferCred) error {
	if err := safeDestination(credential.SrcPath); err != nil {
		return err
	}
	roots := append([]string{a.cfg.TransferDir(), filepath.Join(a.cfg.StateRoot, "recipes")}, a.cfg.CacheRoots...)
	for _, root := range roots {
		if root != "" && pathWithin(credential.SrcPath, root) && credential.SrcPath != root {
			return nil
		}
	}
	return fmt.Errorf("download.peer_source_unconfigured")
}
func (a *Agent) transferCheckpoint(command *agentv1.TransferCommand, credential *transferCred) (string, *peerCheckpoint, error) {
	destination, err := a.transferDestination(command)
	if err != nil {
		return "", nil, err
	}
	binding := command.GetArtifactIdentity() + "\x00" + destination + "\x00" + credential.SourceDigest
	key := sha256.Sum256([]byte(binding))
	staging := filepath.Join(filepath.Dir(destination), ".untrusted-"+hex.EncodeToString(key[:]))
	if err := safeDestination(staging); err != nil {
		return "", nil, err
	}
	checkpoint := &peerCheckpoint{Identity: command.GetArtifactIdentity(), Destination: destination, TreeDigest: credential.SourceDigest, Prefixes: map[string]transferPrefix{}, path: staging + ".checkpoint.json"}
	data, err := os.ReadFile(checkpoint.path)
	if err == nil {
		if len(data) > 16<<20 {
			return "", nil, fmt.Errorf("download.checkpoint_oversized")
		}
		var previous peerCheckpoint
		if json.Unmarshal(data, &previous) != nil || previous.Identity != checkpoint.Identity || previous.Destination != checkpoint.Destination || previous.TreeDigest != checkpoint.TreeDigest {
			return "", nil, fmt.Errorf("download.checkpoint_binding_invalid")
		}
		if previous.Prefixes != nil {
			checkpoint.Prefixes = previous.Prefixes
		}
	} else if !os.IsNotExist(err) {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0755); err != nil {
		return "", nil, err
	}
	if err := checkpoint.save(); err != nil {
		return "", nil, err
	}
	return staging, checkpoint, nil
}
func (c *peerCheckpoint) save() error {
	if err := safeDestination(c.path); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path+".part", data, 0600); err != nil {
		return err
	}
	return os.Rename(c.path+".part", c.path)
}
func (c *peerCheckpoint) openPrefix(ctx context.Context, path, rel string, size uint64, mode os.FileMode) (*os.File, hash.Hash, uint64, error) {
	if err := safeDestination(path); err != nil {
		return nil, nil, 0, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return nil, nil, 0, err
	}
	hash := sha256.New()
	prefix := c.Prefixes[rel]
	if prefix.Offset > size {
		prefix = transferPrefix{}
	}
	if prefix.Offset > 0 {
		n, err := io.CopyN(hash, contextReader{ctx: ctx, reader: file}, int64(prefix.Offset))
		if err != nil || uint64(n) != prefix.Offset || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != prefix.SHA256 {
			prefix = transferPrefix{}
			hash.Reset()
		}
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, nil, 0, err
		}
	}
	if err := file.Truncate(int64(prefix.Offset)); err != nil {
		file.Close()
		return nil, nil, 0, err
	}
	if _, err := file.Seek(int64(prefix.Offset), io.SeekStart); err != nil {
		file.Close()
		return nil, nil, 0, err
	}
	return file, hash, prefix.Offset, nil
}
func (a *Agent) validateTransferredIdentity(ctx context.Context, identity, root string, entries []*agentv1.FileEntry) error {
	parsed, err := artifactidentity.Parse(identity)
	if err != nil {
		return err
	}
	switch parsed.Kind {
	case "file":
		if len(entries) != 1 || entries[0].GetPath() != transferRootFile || !matchesFileDigest(ctx, filepath.Join(root, transferRootFile), parsed.Digest) {
			return fmt.Errorf("download.transfer_file_identity_mismatch")
		}
	case "model":
		snapshot := filepath.Join(root, "snapshots", parsed.Revision)
		if len(hfSnapshotDiagnostics(ctx, identity, root, snapshot)) > 0 {
			return fmt.Errorf("download.transfer_snapshot_identity_mismatch")
		}
	case "recipe":
		packed, err := recipe.ReadLayout(root)
		if err != nil || packed.ManifestDigest != parsed.Digest {
			return fmt.Errorf("download.transfer_package_identity_mismatch")
		}
		if err := recipe.VerifyExtractedAssets(root); err != nil {
			return err
		}
	default:
		return fmt.Errorf("download.transfer_layout_unsupported")
	}
	return ctx.Err()
}
