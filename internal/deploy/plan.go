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
	// ErrUntrusted — the recipe is untrusted and cannot launch. It is
	// inspectable but not launchable until the operator approves it (local)
	// or a signature verifies it. Stable API code: recipe.untrusted (409).
	ErrUntrusted = errors.New("recipe untrusted")
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

// Plan is the previewed deployment (openapi DeploymentPlan).
type Plan struct {
	RecipeDigest  string            `json:"recipe_digest"`
	RecipeName    string            `json:"recipe_name,omitempty"`
	RecipeVersion string            `json:"recipe_version,omitempty"`
	Profile       string            `json:"profile"`
	Variants      map[string]string `json:"variants,omitempty"`
	WorkloadIndex int               `json:"workload_index"`
	Placements    []Placement       `json:"placements"`
	Fabric        *string           `json:"fabric,omitempty"`
	Transfers     []TransferPreview `json:"transfers,omitempty"`
	Ports         []PortPreview     `json:"ports,omitempty"`
	Endpoint      Endpoint          `json:"endpoint,omitempty"`
	Risks         []string          `json:"risks,omitempty"`
	Conflicts     []Conflict        `json:"conflicts,omitempty"`
	Diagnostics   []diag.Diagnostic `json:"diagnostics,omitempty"`
	Ready         bool              `json:"ready"`
	Digest        string            `json:"plan_digest,omitempty"`
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
	PhaseNone      = "none"
	PhasePreparing = "preparing"
	PhasePrepared  = "prepared"
	PhasePulled    = "pulled"
	PhaseCreated   = "created"
	PhaseVerifying = "verifying"
	PhaseStarted   = "started"
	PhaseStopping  = "stopping"
	PhaseStopped   = "stopped"
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

// PlanDigest computes sha256 over the canonical plan JSON minus the digest.
func (p *Plan) PlanDigest() string {
	cp := *p
	cp.Digest = ""
	b, err := cjson.Marshal(cp)
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

// firstNonLoopback returns the first usable unicast address of one node.
// Link-layer (MAC) entries recorded by the host baseline for interfaces
// without a usable IP are skipped: they are not unicast addresses and
// must never become an endpoint host.
func firstNonLoopback(inv *inventory.Inventory) string {
	if inv == nil {
		return ""
	}
	for _, iface := range inv.Interfaces {
		for _, a := range iface.Addresses {
			if a == "" || a == "127.0.0.1" || a == "::1" {
				continue
			}
			host := strings.Split(a, "/")[0]
			if net.ParseIP(host) == nil {
				continue
			}
			return host
		}
	}
	return ""
}
