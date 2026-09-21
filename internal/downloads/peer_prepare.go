package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"path"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// PeerRequest is controller-owned input. An optional address is the enrolled
// source's already-resolved fabric route; TLS still authenticates SourceNode.
// Empty Destination selects the reported canonical writable destination.
type PeerRequest struct {
	SourceNode      string
	DestinationNode string
	Identity        string
	SourcePath      string
	Destination     string
	PeerAddress     string
	RunID           string
	TransferID      string
}

type PreparedPeer struct {
	Command        *agentv1.TransferCommand
	CredentialHash []byte
	TreeSizeBytes  int64
}

// PreparePeer inspects exact source bytes/tree and target capacity, then signs
// the immutable binding used by every new peer transfer. It never dispatches,
// creates a transfer row, or acquires ownership; callers must persist those
// before sending the returned explicit START command.
func (s *Service) PreparePeer(ctx context.Context, request PeerRequest) (*PreparedPeer, error) {
	target, err := s.q.GetNode(ctx, request.DestinationNode)
	if err != nil {
		return nil, err
	}
	spec, err := peerDestination(request.Identity, nodeReport(target), request.Destination)
	if err != nil {
		return nil, err
	}
	resource := Resource{ResourceSpec: spec, NodeID: request.DestinationNode, SourceNode: request.SourceNode, SourcePath: request.SourcePath, Action: ActionPeerCopy, Required: true}
	return s.preparePeer(ctx, resource, request.PeerAddress, request.RunID, request.TransferID, nil)
}

func validPeerAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && host != "" && port != "" && host != "0.0.0.0" && host != "::"
}

func (s *Service) preparePeer(ctx context.Context, resource Resource, address, runID, transferID string, credentials []CredentialSelection) (*PreparedPeer, error) {
	if resource.NodeID == resource.SourceNode || resource.NodeID == "" || resource.SourceNode == "" || transferID == "" || runID == "" {
		return nil, failure("download.peer_invalid", "Distinct enrolled source/destination and durable attempt IDs are required", 422)
	}
	source, err := s.q.GetNode(ctx, resource.SourceNode)
	if err != nil {
		return nil, err
	}
	target, err := s.q.GetNode(ctx, resource.NodeID)
	if err != nil {
		return nil, err
	}
	if !s.nodes.Online(source.ID) || !s.nodes.Online(target.ID) {
		return nil, failure("download.plan_changed", "Reviewed peer source or destination is offline", 412)
	}
	sourceInventory, targetInventory := nodeReport(source), nodeReport(target)
	if !hasFeature(sourceInventory) || !hasFeature(targetInventory) {
		return nil, failure("download.agent_upgrade_required", "Both peer devices must advertise downloads-v1", 422)
	}
	canonical, err := peerDestination(resource.Identity, targetInventory, resource.Destination)
	if err != nil {
		return nil, err
	}
	if canonical.Kind != resource.Kind || canonical.Source.Type != resource.Source.Type && resource.Source.Type != SourceLocal {
		return nil, failure("download.peer_invalid", "Peer resource kind does not match the canonical immutable identity", 422)
	}
	if address == "" {
		address = sourceInventory.PeerListen
	}
	if !validPeerAddress(address) {
		return nil, failure("download.peer_unavailable", "Peer did not report a routable authenticated transfer address", 422)
	}
	sourceSpec := resource.ResourceSpec
	sourceSpec.Destination = resource.SourcePath
	if err = sourceSpec.Validate(); err != nil {
		return nil, err
	}
	observed, err := s.inspect(ctx, source.ID, sourceSpec, credentials)
	if err != nil {
		return nil, err
	}
	if observed.State != ResourceAvailable || observed.VerifiedAt == nil || observed.TreeDigest == "" || observed.TreeSizeBytes == nil || observed.SizeBytes == nil {
		return nil, failure("download.plan_changed", "Peer no longer has verified exact content and transfer-tree metadata", 412)
	}
	// Logical size is independently established by exact source inspection. It is
	// distinct from the transfer tree (which may include snapshot/record entries).
	destinationSpec := resource.ResourceSpec
	destinationSpec.SizeBytes = observed.SizeBytes
	destination, err := s.inspect(ctx, target.ID, destinationSpec, credentials)
	if err != nil {
		return nil, err
	}
	treeSize := *observed.TreeSizeBytes
	if destination.Storage == nil || destination.Storage.AvailableBytes == nil || treeSize < 0 || treeSize > (1<<62)-StorageReserveBytes || *destination.Storage.AvailableBytes < 2*treeSize+StorageReserveBytes {
		return nil, failure("download.storage_changed", "Destination lacks known safe capacity for the verified peer tree and staging", 412)
	}
	if s.ca == nil {
		return nil, failure("download.peer_unavailable", "Peer transfer signing authority is unavailable", 503)
	}
	credential := peerCredential{TransferID: transferID, RunID: runID, SourceNode: source.ID, DestNode: target.ID, ArtifactID: resource.Identity, SrcPath: resource.SourcePath, SourceDigest: observed.TreeDigest, SrcSize: treeSize, DestPath: resource.Destination, ExpUnix: time.Now().Add(30 * time.Minute).Unix(), Identity: resource.Identity, TreeDigest: observed.TreeDigest, Operation: "start"}
	credential.Signature, err = s.ca.SignCA([]byte(encoded(credential)))
	if err != nil {
		return nil, err
	}
	raw := []byte(encoded(credential))
	sum := sha256.Sum256(raw)
	return &PreparedPeer{Command: &agentv1.TransferCommand{TransferId: transferID, Op: agentv1.TransferOp_TRANSFER_OP_START, Role: "dest", PeerAddress: address, Credential: base64.StdEncoding.EncodeToString(raw), ArtifactIdentity: resource.Identity, SrcPath: resource.SourcePath, DestPath: resource.Destination, TimeoutSeconds: 3600}, CredentialHash: sum[:], TreeSizeBytes: treeSize}, nil
}

func peerDestination(identity string, inventory nodeInventory, destination string) (ResourceSpec, error) {
	parsed, err := artifactidentity.Parse(identity)
	if err != nil {
		return ResourceSpec{}, err
	}
	spec := ResourceSpec{Identity: identity, Kind: ResourceArtifact}
	var destinations []string
	switch parsed.Kind {
	case "model":
		spec.Source.Type = SourceHuggingFace
		spec.Source.Reference, spec.Source.Revision, _ = strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
		model := "models--" + strings.ReplaceAll(spec.Source.Reference, "/", "--")
		for _, root := range inventory.CacheRoots {
			if root.Writable {
				destinations = append(destinations, path.Join(root.Path, "hub", model), path.Join(root.Path, model))
			}
		}
	case "file":
		spec.Source = SourceSpec{Type: SourceFile, Digest: parsed.Digest}
		for _, root := range inventory.CacheRoots {
			if root.Writable {
				destinations = append(destinations, path.Join(root.Path, "files", strings.TrimPrefix(parsed.Digest, "sha256:")))
			}
		}
	case "recipe":
		spec.Kind = ResourceRecipe
		spec.Source = SourceSpec{Type: SourceRecipe, Digest: parsed.Digest}
		if inventory.DownloadRoots.RecipeRoot != "" {
			destinations = append(destinations, path.Join(inventory.DownloadRoots.RecipeRoot, strings.TrimPrefix(parsed.Digest, "sha256:")))
		}
	default:
		return ResourceSpec{}, failure("download.artifact_layout_unsupported", "Peer acquisition has no declared supported artifact layout for this identity", 422)
	}
	if len(destinations) == 0 {
		return spec, failure("download.storage_unavailable", "Device has no reported writable canonical destination for this artifact", 422)
	}
	if destination == "" {
		destination = destinations[0]
	}
	allowed := false
	for _, candidate := range destinations {
		if destination == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return spec, failure("download.destination_invalid", "Peer destination must be the canonical artifact path under a reported configured root", 422)
	}
	spec.Destination = destination
	return spec, spec.Validate()
}
