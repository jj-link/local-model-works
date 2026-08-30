// Package deploy owns the serving deployment domain: inventory-based
// planning (placement, artifacts, ports, risks, conflicts), transactional
// creation with exact-resource leases, per-rank sequential workload
// dispatch, stop/verify, and crash/reconnect convergence.
package deploy

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/inventory"
)

// Sentinel errors; the API layer maps these to status codes.
var (
	ErrUnknown   = errors.New("unknown deployment")
	ErrRecipe    = errors.New("unknown recipe")
	ErrProfile   = errors.New("unknown profile")
	ErrNoTarget  = errors.New("no suitable nodes")
	ErrConflict  = errors.New("resource conflict")
	ErrPlanStale = errors.New("plan digest mismatch")
	ErrNotReady  = errors.New("plan not ready")
	ErrState     = errors.New("invalid state for operation")
)

// Placement is one rank assignment (openapi placements item).
type Placement struct {
	NodeID           string   `json:"node_id"`
	NodeName         string   `json:"node_name,omitempty"`
	Rank             int32    `json:"rank"`
	AcceleratorIndex int32    `json:"accelerator_index"`
	AcceleratorUUID  string   `json:"accelerator_uuid,omitempty"`
	Accelerators     []string `json:"accelerators,omitempty"` // full set for multi-GPU ranks
	Container        string   `json:"container,omitempty"`
}

// PlacementOverride is an operator-pinned rank->node (create request).
type PlacementOverride struct {
	NodeID string `json:"node_id"`
	Rank   int32  `json:"rank"`
}

// PortPreview is one published port (openapi plan ports item).
type PortPreview struct {
	NodeID        string `json:"node_id"`
	NodeName      string `json:"node_name,omitempty"`
	HostPort      int32  `json:"host_port"`
	ContainerPort int32  `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"`
}

// Endpoint is the serving endpoint (rank 0).
type Endpoint struct {
	Host  string `json:"host,omitempty"`
	Port  int32  `json:"port,omitempty"`
	Path  string `json:"path,omitempty"`
	Model string `json:"model,omitempty"`
}

// Conflict is one blocking resource conflict.
type Conflict struct {
	Resource     string `json:"resource"`
	OccupiedBy   string `json:"occupied_by,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
}

// TransferPreview previews one artifact move (openapi).
type TransferPreview struct {
	ArtifactID string `json:"artifact_id"`
	Identity   string `json:"identity,omitempty"`
	SourceNode string `json:"source_node"`
	SourcePath string `json:"source_path,omitempty"`
	DestNode   string `json:"dest_node"`
	DestPath   string `json:"dest_path"`
	Bytes      int64  `json:"bytes"`
	Network    string `json:"network,omitempty"`
}

// ImagePreview is the immutable image each selected node will verify/pull.
type ImagePreview struct {
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name,omitempty"`
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
	Action    string `json:"action"`
}

// StoragePreview compares missing artifact bytes with the live filesystem
// that owns the node's configured cache root.
type StoragePreview struct {
	NodeID         string `json:"node_id"`
	NodeName       string `json:"node_name,omitempty"`
	CacheRoot      string `json:"cache_root,omitempty"`
	RequiredBytes  int64  `json:"required_bytes"`
	AvailableBytes int64  `json:"available_bytes,omitempty"`
	TotalBytes     int64  `json:"total_bytes,omitempty"`
	Known          bool   `json:"known"`
	Sufficient     bool   `json:"sufficient"`
}

// HostPreparationPreview makes privileged-but-bounded memory preparation
// explicit before launch.
type HostPreparationPreview struct {
	NodeID            string `json:"node_id"`
	NodeName          string `json:"node_name,omitempty"`
	RequireSwap       bool   `json:"require_swap"`
	SwapTotalBytes    int64  `json:"swap_total_bytes,omitempty"`
	SwappinessCurrent uint32 `json:"swappiness_current"`
	SwappinessTarget  *int   `json:"swappiness_target,omitempty"`
	DropPageCache     bool   `json:"drop_page_cache"`
	HelperImage       string `json:"helper_image"`
}

// Plan is the previewed deployment (openapi DeploymentPlan).
type Plan struct {
	RecipeDigest    string                   `json:"recipe_digest"`
	RecipeName      string                   `json:"recipe_name,omitempty"`
	RecipeVersion   string                   `json:"recipe_version,omitempty"`
	Profile         string                   `json:"profile"`
	Variants        map[string]string        `json:"variants,omitempty"`
	WorkloadIndex   int                      `json:"workload_index"`
	Placements      []Placement              `json:"placements"`
	Fabric          *string                  `json:"fabric,omitempty"`
	Transfers       []TransferPreview        `json:"transfers,omitempty"`
	Images          []ImagePreview           `json:"images,omitempty"`
	Storage         []StoragePreview         `json:"storage,omitempty"`
	HostPreparation []HostPreparationPreview `json:"host_preparation,omitempty"`
	Ports           []PortPreview            `json:"ports,omitempty"`
	Endpoint        Endpoint                 `json:"endpoint,omitempty"`
	Risks           []string                 `json:"risks,omitempty"`
	Conflicts       []Conflict               `json:"conflicts,omitempty"`
	Diagnostics     []diag.Diagnostic        `json:"diagnostics,omitempty"`
	Ready           bool                     `json:"ready"`
	Digest          string                   `json:"plan_digest,omitempty"`
}

// PlanRequest previews a deployment (openapi DeploymentPlanRequest).
type PlanRequest struct {
	RecipeDigest string              `json:"recipe_digest"`
	Profile      string              `json:"profile"`
	Placements   []PlacementOverride `json:"placements,omitempty"`
	// Variants maps artifact name -> selected variant name. When an artifact
	// declares variants, the chosen variant's source identity is used for
	// placement planning. Empty selects the artifact's defaultVariant.
	Variants map[string]string `json:"variants,omitempty"`
}

// CreateRequest creates from a validated plan (openapi).
type CreateRequest struct {
	RecipeDigest string              `json:"recipe_digest"`
	Profile      string              `json:"profile"`
	Placements   []PlacementOverride `json:"placements,omitempty"`
	PlanDigest   string              `json:"plan_digest,omitempty"`
	Variants     map[string]string   `json:"variants,omitempty"`
}

// dispatchPhase is one rank's completed dispatch step.
type dispatchPhases map[int32]string

const (
	PhaseNone          = "none"
	PhasePreparing     = "preparing"
	PhasePrepared      = "prepared"
	PhasePulled        = "pulled"
	PhaseCreated       = "created"
	PhaseHostPreparing = "host_preparing"
	PhaseHostPrepared  = "host_prepared"
	PhaseVerifying     = "verifying"
	PhaseStarted       = "started"
	PhaseStopping      = "stopping"
	PhaseStopped       = "stopped"
)

func (d dispatchPhases) Get(rank int32) string {
	if p, ok := d[rank]; ok && p != "" {
		return p
	}
	return PhaseNone
}

// ParseDispatch decodes the deployments.dispatch column.
func ParseDispatch(raw string) dispatchPhases {
	out := dispatchPhases{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// PlanDigest identifies the launch contract the operator reviewed. Live
// preflight telemetry (free bytes, cache/download actions, host readings, and
// diagnostics) is deliberately excluded: Create recomputes and enforces that
// telemetry, but harmless changes between preview and click must not make an
// otherwise identical launch stale.
func (p *Plan) PlanDigest() string {
	contract := struct {
		RecipeDigest  string            `json:"recipe_digest"`
		Profile       string            `json:"profile"`
		Variants      map[string]string `json:"variants,omitempty"`
		WorkloadIndex int               `json:"workload_index"`
		Placements    []Placement       `json:"placements"`
		Fabric        *string           `json:"fabric,omitempty"`
		Ports         []PortPreview     `json:"ports,omitempty"`
		Endpoint      Endpoint          `json:"endpoint,omitempty"`
	}{
		RecipeDigest: p.RecipeDigest, Profile: p.Profile, Variants: p.Variants,
		WorkloadIndex: p.WorkloadIndex, Placements: p.Placements, Fabric: p.Fabric,
		Ports: p.Ports, Endpoint: p.Endpoint,
	}
	b, err := cjson.Marshal(contract)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LeaseResources renders the exact resource identities a deployment holds.
func (p *Plan) LeaseResources() []string {
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, pl := range p.Placements {
		if len(pl.Accelerators) > 0 {
			for _, u := range pl.Accelerators {
				add("gpu:" + pl.NodeID + ":" + u)
			}
		} else if pl.AcceleratorUUID != "" {
			add("gpu:" + pl.NodeID + ":" + pl.AcceleratorUUID)
		}
	}
	if p.Fabric != nil && *p.Fabric != "" {
		add("fabric:" + *p.Fabric)
	}
	for _, port := range p.Ports {
		add(fmt.Sprintf("port:%s:%d", port.NodeID, port.HostPort))
	}
	sort.Strings(out)
	return out
}

// nodeCandidate is one schedulable node with its evaluated inventory.
type nodeCandidate struct {
	NodeID   string
	NodeName string
	Status   string
	Inv      *inventory.Inventory
	// free accelerators matching the requirement, index order.
	Free []inventory.Accelerator
	// total matching (leased or not), for capacity reporting.
	Matching int
}

// planAccRequirement is the accelerator predicate for one plan.
type planAccRequirement struct {
	Required      bool
	Vendor        string
	Architectures []string
	MinMemory     int64
	Features      []string
	Count         int
	All           bool
}

func matchesAcc(req *planAccRequirement, a inventory.Accelerator) bool {
	if req.Vendor != "" && a.Vendor != req.Vendor {
		return false
	}
	if len(req.Architectures) > 0 {
		ok := false
		for _, arch := range req.Architectures {
			if a.Architecture == arch {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if req.MinMemory > 0 && int64(a.MemoryBytes) < req.MinMemory {
		return false
	}
	for _, f := range req.Features {
		has := false
		for _, af := range a.Features {
			if af == f {
				has = true
			}
		}
		if !has {
			return false
		}
	}
	return true
}

// nodeInfo is the planning view of one nodes row.
type nodeInfo struct {
	ID          string
	DisplayName string
	Status      string
	Inventory   sql.NullString
}

// firstNonLoopback returns a usable controller-facing address. A Tailscale
// CGNAT address is preferred when present; fabric-only and container bridge
// addresses may not be reachable from the operator console.
func firstNonLoopback(inv *inventory.Inventory) string {
	if inv == nil {
		return ""
	}
	var fallback string
	for _, iface := range inv.Interfaces {
		for _, address := range iface.Addresses {
			host := strings.Split(address, "/")[0]
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			v4 := ip.To4()
			if v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
				return host
			}
			if fallback == "" {
				fallback = host
			}
		}
	}
	return fallback
}
