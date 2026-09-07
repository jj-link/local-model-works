// Package downloads defines acquisition contracts shared by the controller and
// device agent. It must not depend on agent or deploy: downloading bytes is not
// permission to execute a recipe, prepare a host, or create a workload.
package downloads

import (
	"time"

	"github.com/jj-link/local-model-works/internal/diag"
)

const (
	ProtocolFeature = "downloads-v1"
	JobKind         = "recipe-download"
	// The controller declares this immutable helper; download may pull but never execute it.
	HostPreparationImage           = "docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"
	MaxResourceJSONBytes           = 16 * 1024
	MaxCredentialJSONBytes         = 32 * 1024
	MaxItemResourceJSONBytes       = 64 * 1024
	MaxCheckpointJSONBytes         = 64 * 1024
	MaxItemErrorJSONBytes          = 16 * 1024
	MaxIdentityBytes               = 2048
	MaxDestinationBytes            = 4096
	MaxSourceURLBytes              = 4096
	MaxTargets                     = 256
	MaxResources                   = 4096
	MaxCredentials                 = 256
	StorageReserveBytes      int64 = 5 * 1024 * 1024 * 1024
)

type ResourceKind string

const (
	ResourceArtifact ResourceKind = "artifact"
	ResourceRecipe   ResourceKind = "recipe"
	ResourceImage    ResourceKind = "image"
)

type SourceType string

const (
	SourceHuggingFace SourceType = "huggingface"
	SourceOCI         SourceType = "oci"
	SourceFile        SourceType = "file"
	SourceLocal       SourceType = "local"
	SourceRecipe      SourceType = "recipe"
)

// SourceSpec names only immutable data. Reference is the HF owner/repository or
// canonical OCI repository, never a command or local source path. URL is allowed
// only for a checksum-pinned HTTPS file. Recipe sources use the authenticated
// controller package transport; local sources cannot fetch from an origin.
type SourceSpec struct {
	Type      SourceType `json:"type"`
	Reference string     `json:"reference,omitempty"`
	URL       string     `json:"url,omitempty"`
	Revision  string     `json:"revision,omitempty"`
	Digest    string     `json:"digest,omitempty"`
}

// ResourceSpec is the complete, bounded DownloadCommand.resource_json contract.
// DecodeResourceSpec rejects unknown fields and unpinned/contradictory identities.
// Destination is an exact resolved path, NOT a caller-selected filesystem root.
// The receiver must additionally match it to its configured cache/package root,
// or actual image-engine storage, and reject symlink/containment escapes. Image
// commands use the engine API, never direct writes into engine storage.
// IndexDigest records the selected immutable index (or manifest for a non-index
// reference); ManifestDigest records the selected platform-specific manifest.
type ResourceSpec struct {
	Kind           ResourceKind `json:"kind"`
	Identity       string       `json:"identity"`
	Source         SourceSpec   `json:"source"`
	Destination    string       `json:"destination"`
	Platform       string       `json:"platform,omitempty"`
	IndexDigest    string       `json:"index_digest,omitempty"`
	ManifestDigest string       `json:"manifest_digest,omitempty"`
	SizeBytes      *int64       `json:"size_bytes,omitempty"`
}

type Action string

const (
	ActionReuse          Action = "reuse"
	ActionValidateLocal  Action = "validate-local"
	ActionDownloadOrigin Action = "download-origin"
	ActionPeerCopy       Action = "peer-copy"
)

type ResourceState string

const (
	ResourceUnknown   ResourceState = "unknown"
	ResourceMissing   ResourceState = "missing"
	ResourcePartial   ResourceState = "partial"
	ResourceVerifying ResourceState = "verifying"
	ResourceAvailable ResourceState = "available"
	ResourceInvalid   ResourceState = "invalid"
)

// Verification is observation, not identity authority. Available is justified
// only by exact-source bytes/package layers/image platform verification. An
// offline or stale observation must not authorize reuse without a fresh check.
type Verification struct {
	State       ResourceState     `json:"state"`
	VerifiedAt  *time.Time        `json:"verified_at,omitempty"`
	Stale       bool              `json:"stale"`
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

type Resource struct {
	ResourceSpec
	Key            string       `json:"key"`
	NodeID         string       `json:"node_id"`
	Action         Action       `json:"action"`
	SourceNode     string       `json:"source_node,omitempty"`
	SourcePath     string       `json:"source_path,omitempty"`
	BytesTotal     *int64       `json:"bytes_total,omitempty"`
	BytesRemaining *int64       `json:"bytes_remaining,omitempty"`
	CredentialID   string       `json:"credential_id,omitempty"`
	Verification   Verification `json:"verification"`
	Required       bool         `json:"required"`
}

// Target identifies an enrolled node, never a display name. CacheRoot, when
// supplied, must equal a reported configured writable root. Planning freezes
// the resolved default root so later commands cannot silently choose another.
type Target struct {
	NodeID    string `json:"node_id"`
	CacheRoot string `json:"cache_root,omitempty"`
}

// CredentialSelection contains IDs only. Resource is a canonical immutable
// resource identity; Host is the exact approved credential destination host.
type CredentialSelection struct {
	Resource string `json:"resource"`
	Host     string `json:"host"`
	SecretID string `json:"secret_id"`
}

// CredentialMaterial is transient DownloadCommand.credential_json, never part
// of a plan, item, checkpoint, run input, event or log. Value preserves the
// selected secret's existing format; transports must reject incompatible
// formats rather than guessing a conversion. Resolve only the selected host.
type CredentialMaterial struct {
	Purpose string `json:"purpose"` // huggingface | registry
	Host    string `json:"host"`
	Value   string `json:"value"`
}

// PlanRequest requires explicit, unique nonempty targets. A launch profile and
// explicit variants/workload selection are mutually exclusive. A nil index is
// not index zero: ambiguous runtimes require an explicit selection. ResumeRunID
// freezes content/targets to that interrupted/cancelled/failed attempt.
type PlanRequest struct {
	RecipeDigest    string                `json:"recipe_digest"`
	Targets         []Target              `json:"targets"`
	WorkloadIndex   *int                  `json:"workload_index,omitempty"`
	Variants        map[string]string     `json:"variants,omitempty"`
	LaunchProfileID string                `json:"launch_profile_id,omitempty"`
	Credentials     []CredentialSelection `json:"credentials,omitempty"`
	ResumeRunID     string                `json:"resume_run_id,omitempty"`
	// ReviewedPlan is serving's persisted acquisition contract, not client JSON.
	ReviewedPlan *Plan `json:"-"`
}

type CreateRequest struct {
	PlanRequest
	PlanDigest string `json:"plan_digest"`
}

// Resume cannot change content or devices. It requires a freshly reviewed plan
// for the predecessor run; replacement credential IDs are the only new choices.
type ResumeRequest struct {
	RecipeDigest string                `json:"recipe_digest"`
	PlanDigest   string                `json:"plan_digest"`
	Credentials  []CredentialSelection `json:"credentials,omitempty"`
}

type AvailabilityRequest struct {
	RecipeDigest    string            `json:"recipe_digest"`
	Targets         []Target          `json:"targets,omitempty"` // omitted means all enrolled devices
	WorkloadIndex   *int              `json:"workload_index,omitempty"`
	Variants        map[string]string `json:"variants,omitempty"`
	LaunchProfileID string            `json:"launch_profile_id,omitempty"`
	Refresh         bool              `json:"refresh,omitempty"` // bounded inspect only, never acquisition
}

// Storage is per actual filesystem, not per artifact or configured path alias.
// Nil sizes/capacity/sufficiency mean unknown, never zero or enough. Required
// bytes include shared-resource deduplication and staging, excluding ReserveBytes.
type Storage struct {
	NodeID         string   `json:"node_id"`
	Filesystem     string   `json:"filesystem"`
	Destination    string   `json:"destination"`
	ResourceKeys   []string `json:"resource_keys"`
	RequiredBytes  *int64   `json:"required_bytes,omitempty"`
	StagingBytes   *int64   `json:"staging_bytes,omitempty"`
	AvailableBytes *int64   `json:"available_bytes,omitempty"`
	TotalBytes     *int64   `json:"total_bytes,omitempty"`
	ReserveBytes   int64    `json:"reserve_bytes"`
	Sufficient     *bool    `json:"sufficient,omitempty"`
}

// PlanDigest binds normalized choices, all resource identities, destinations,
// platforms, credential IDs and action/peer selection, not observations, free
// space or timestamps. Submit rechecks capacity and source availability; an
// action/source change requires another review, never silent fallback.
type Plan struct {
	RecipeDigest  string            `json:"recipe_digest"`
	WorkloadIndex int               `json:"workload_index"`
	Variants      map[string]string `json:"variants"`
	Targets       []Target          `json:"targets"`
	Resources     []Resource        `json:"resources"`
	Storage       []Storage         `json:"storage"`
	Diagnostics   []diag.Diagnostic `json:"diagnostics"`
	Ready         bool              `json:"ready"`
	PlanDigest    string            `json:"plan_digest"`
}

type AvailableResource struct {
	Key            string       `json:"key"`
	Kind           ResourceKind `json:"kind"`
	Identity       string       `json:"identity"`
	Destination    string       `json:"destination"`
	Platform       string       `json:"platform,omitempty"`
	IndexDigest    string       `json:"index_digest,omitempty"`
	ManifestDigest string       `json:"manifest_digest,omitempty"`
	Required       bool         `json:"required"`
	Verification   Verification `json:"verification"`
}

// OtherVersion is informational. Its presence never satisfies another identity;
// SupportedVariants, if any, may be explicitly selected to request a new plan.
type OtherVersion struct {
	Kind              ResourceKind      `json:"kind"`
	Identity          string            `json:"identity"`
	Path              string            `json:"path"`
	Verification      Verification      `json:"verification"`
	SupportedVariants map[string]string `json:"supported_variants,omitempty"`
}

type RunningDeployment struct {
	DeploymentID  string            `json:"deployment_id"`
	RecipeDigest  string            `json:"recipe_digest"`
	WorkloadIndex int               `json:"workload_index"`
	Variants      map[string]string `json:"variants"`
	Rank          int32             `json:"rank"`
	State         string            `json:"state"`
}

type DeviceAvailability struct {
	NodeID             string              `json:"node_id"`
	NodeName           string              `json:"node_name"`
	Online             bool                `json:"online"`
	LastCheckedAt      *time.Time          `json:"last_checked_at,omitempty"`
	Resources          []AvailableResource `json:"resources"`
	OtherVersions      []OtherVersion      `json:"other_versions"`
	ActiveRunIDs       []string            `json:"active_run_ids"`
	RunningDeployments []RunningDeployment `json:"running_deployments"`
	RuntimeDiagnostics []diag.Diagnostic   `json:"runtime_diagnostics"`
	Diagnostics        []diag.Diagnostic   `json:"diagnostics"`
}

type Availability struct {
	RecipeDigest  string               `json:"recipe_digest"`
	WorkloadIndex int                  `json:"workload_index"`
	Variants      map[string]string    `json:"variants"`
	Devices       []DeviceAvailability `json:"devices"`
	Diagnostics   []diag.Diagnostic    `json:"diagnostics"`
}

type ItemState string

const (
	ItemPending      ItemState = "pending"
	ItemChecking     ItemState = "checking"
	ItemTransferring ItemState = "transferring"
	ItemVerifying    ItemState = "verifying"
	ItemCancelling   ItemState = "cancelling"
	ItemSucceeded    ItemState = "succeeded"
	ItemFailed       ItemState = "failed"
	ItemCancelled    ItemState = "cancelled"
	ItemInterrupted  ItemState = "interrupted"
)

type Progress struct {
	ItemID      string  `json:"item_id"`
	CommandID   string  `json:"command_id"`
	Phase       string  `json:"phase"`
	BytesDone   uint64  `json:"bytes_done"`
	BytesTotal  *uint64 `json:"bytes_total,omitempty"`
	FilesDone   uint32  `json:"files_done"`
	FilesTotal  *uint32 `json:"files_total,omitempty"`
	CurrentFile string  `json:"current_file,omitempty"`
}

// Checkpoint references agent-owned durable state; it is not permission to use
// a path supplied by a controller. Resume must revalidate all retained bytes.
// Engine-managed layers may be reusable without byte-exact image progress.
type Checkpoint struct {
	AgentCheckpointID string    `json:"agent_checkpoint_id,omitempty"`
	Progress          *Progress `json:"progress,omitempty"`
	SourceTreeDigest  string    `json:"source_tree_digest,omitempty"`
}

type ItemError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Item struct {
	ID                string     `json:"id"`
	RunID             string     `json:"run_id"`
	PredecessorItemID string     `json:"predecessor_item_id,omitempty"`
	ResourceKey       string     `json:"resource_key"`
	NodeID            string     `json:"node_id"`
	Resource          Resource   `json:"resource"`
	State             ItemState  `json:"state"`
	CommandID         string     `json:"command_id,omitempty"`
	TransferID        string     `json:"transfer_id,omitempty"`
	Checkpoint        Checkpoint `json:"checkpoint"`
	Error             *ItemError `json:"error,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// AttemptInput is persisted once in runs.input before any device command.
// Resume creates a new run and new items linked to predecessors; it never resets
// terminal rows or replays acquisition automatically after reconnect/restart.
type AttemptInput struct {
	Plan             Plan                  `json:"plan"`
	Credentials      []CredentialSelection `json:"credentials,omitempty"`
	PredecessorRunID string                `json:"predecessor_run_id,omitempty"`
}

type Attempt struct {
	RunID      string       `json:"run_id"`
	State      string       `json:"state"` // existing runs state contract
	Input      AttemptInput `json:"input"`
	Items      []Item       `json:"items"`
	CreatedAt  time.Time    `json:"created_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
}

// CommandOutput is CommandResult.output_json. Identity, path and platform must
// match the enrolled sender's active item before success; late results cannot
// revive terminal state. CANCEL returns only after work is quiescent. INSPECT
// can report missing/invalid without an execution failure (CommandResult.ok).
type CommandOutput struct {
	ItemID         string              `json:"item_id"`
	Identity       string              `json:"identity"`
	Path           string              `json:"path"`
	Platform       string              `json:"platform,omitempty"`
	VerifiedAt     *time.Time          `json:"verified_at,omitempty"`
	State          ResourceState       `json:"state"`
	SizeBytes      *int64              `json:"size_bytes,omitempty"`
	Storage        *StorageObservation `json:"storage,omitempty"`
	TreeDigest     string              `json:"tree_digest,omitempty"`
	TreeSizeBytes  *int64              `json:"tree_size_bytes,omitempty"`
	IndexDigest    string              `json:"index_digest,omitempty"`
	ManifestDigest string              `json:"manifest_digest,omitempty"`
	BytesRemaining *int64              `json:"bytes_remaining,omitempty"`
}

// StorageObservation is a fresh agent-side stat of the resource's actual
// filesystem. Values remain optional when the engine or filesystem cannot report.
type StorageObservation struct {
	Filesystem     string `json:"filesystem"`
	Destination    string `json:"destination"`
	AvailableBytes *int64 `json:"available_bytes,omitempty"`
	TotalBytes     *int64 `json:"total_bytes,omitempty"`
}

// Destination locks are shared by download, serving and standalone transfer
// owners. Terminal/interrupted run state does NOT release a writer. The owner
// first records a terminal acknowledgement or node quiescence barrier; only
// then may the lock be removed/reacquired in a transaction. There is no expiry.
type LockState string

const (
	LockHeld       LockState = "held"
	LockCancelling LockState = "cancelling"
	LockQuiesced   LockState = "quiesced"
)

type OwnerKind string

const (
	OwnerDownload OwnerKind = "download"
	OwnerServing  OwnerKind = "serving"
	OwnerTransfer OwnerKind = "transfer"
)

type QuiescenceProof string

const (
	QuiescenceTerminalAck     QuiescenceProof = "terminal-ack"
	QuiescenceNodeBarrier     QuiescenceProof = "node-barrier"
	QuiescenceNeverDispatched QuiescenceProof = "never-dispatched"
)

type DestinationLock struct {
	NodeID          string          `json:"node_id"`
	Destination     string          `json:"destination"`
	Identity        string          `json:"identity"`
	Platform        string          `json:"platform,omitempty"`
	OwnerKind       OwnerKind       `json:"owner_kind"`
	OwnerID         string          `json:"owner_id"`
	RunID           string          `json:"run_id,omitempty"`
	ItemID          string          `json:"item_id,omitempty"`
	State           LockState       `json:"state"`
	QuiescedAt      *time.Time      `json:"quiesced_at,omitempty"`
	QuiescenceProof QuiescenceProof `json:"quiescence_proof,omitempty"`
}
