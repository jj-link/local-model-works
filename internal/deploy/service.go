package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// NodeSender delivers commands to one node and reports liveness.
type NodeSender interface {
	Send(nodeID string, m *agentv1.ServerMessage) bool
	Online(nodeID string) bool
}

// inflightCmd correlates a command_id to its deployment/rank/operation.
type inflightCmd struct {
	DepID string
	Rank  int32
	Op    string
}

// Service is the deployment domain service.
type Service struct {
	db    *sql.DB
	q     *db.Queries
	bus   *events.EventBus
	runs  *runs.Service
	nodes NodeSender
	ca    *ca.CA

	mu       sync.Mutex
	inflight map[string]*inflightCmd
	// transferInflight keys "depID|nodeID|artifactIdentity" to the active
	// transfer id; dispatch stays gated until a placement or ack resolves it.
	transferInflight map[string]string
	dispatchMu       sync.Mutex // serializes dispatch-phase read-modify-write
}

func New(dbh *sql.DB, q *db.Queries, bus *events.EventBus, runsSvc *runs.Service, nodes NodeSender, ca *ca.CA) *Service {
	return &Service{
		db:               dbh,
		q:                q,
		bus:              bus,
		runs:             runsSvc,
		nodes:            nodes,
		ca:               ca,
		inflight:         map[string]*inflightCmd{},
		transferInflight: map[string]string{},
	}
}

// ---------------------------------------------------------------- planning

// Plan previews a deployment from the current fleet state.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (*Plan, error) {
	row, err := s.q.GetRecipe(ctx, req.RecipeDigest)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRecipe, req.RecipeDigest)
	}
	// Trust gate: an untrusted recipe is inspectable but not launchable.
	// Plan (and thus Create) blocks before any run or deployment row exists.
	if row.TrustState == recipe.TrustUntrusted {
		return nil, fmt.Errorf("%w: %s: approve the permission diff or verify a signature first", ErrUntrusted, req.RecipeDigest)
	}
	m, err := recipe.Parse([]byte(row.Manifest))
	if err != nil {
		return nil, fmt.Errorf("recipe manifest: %w", err)
	}
	values, err := m.ProfileValues(req.Profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProfile, err)
	}

	plan := &Plan{
		RecipeDigest:  req.RecipeDigest,
		RecipeName:    row.Name,
		RecipeVersion: row.Version,
		Profile:       req.Profile,
	}

	nodes, err := s.q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	leased := map[string]bool{}
	leases, err := s.q.ActiveLeases(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range leases {
		leased[r] = true
	}

	reqAcc := s.accelRequirement(m)

	// Try workload variants in order; the first one the fleet can satisfy wins.
	var (
		wi  int
		w   *recipe.Workload
		try bool
	)
	for i := range m.Workloads {
		variant := &m.Workloads[i]
		nodeCount := s.workloadNodeCount(m, variant)
		target := recipe.Target{NodeCount: nodeCount}
		if reqAcc.Required {
			v, arch, feats := s.fleetAccProfile(nodes)
			target.Vendor = v
			target.Architecture = arch
			target.Features = feats
		}
		ok, err := variant.Match.SatisfiedBy(target)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		wi, w, try = i, variant, true
		break
	}
	if !try {
		return nil, fmt.Errorf("%w: no workload variant matches the fleet", ErrNoTarget)
	}
	plan.WorkloadIndex = wi
	devAll := false
	devIndices := []int{}
	if dev := w.Devices; dev != nil && dev.Accelerator != nil {
		devAll = dev.Accelerator.All
		devIndices = dev.Accelerator.Indices
	}

	// Candidates: online nodes with enough free accelerators.
	type candidate struct {
		nodeID   string
		nodeName string
		addr     string
		inv      *inventory.Inventory
		free     []inventory.Accelerator
	}
	var candidates []candidate
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		var inv *inventory.Inventory
		if n.Inventory.Valid && n.Inventory.String != "" {
			if perr := json.Unmarshal([]byte(n.Inventory.String), &inv); perr != nil || inv == nil {
				inv = nil
			}
		}
		if reqAcc.Required && inv == nil {
			continue
		}
		c := candidate{
			nodeID:   n.ID,
			nodeName: n.DisplayName,
			addr:     firstNonLoopback(inv),
			inv:      inv,
		}
		if c.addr == "" {
			c.addr = c.nodeName
		}
		if reqAcc.Required {
			for _, a := range inv.Accelerators {
				if !matchesAcc(reqAcc, a) {
					continue
				}
				if !leased["gpu:"+n.ID+":"+a.UUID] {
					c.free = append(c.free, a)
				}
			}
			needed := reqAcc.Count
			if devAll {
				needed = 1
			}
			if len(c.free) < needed {
				continue
			}
			if len(devIndices) > 0 {
				ok := true
				for _, idx := range devIndices {
					has := false
					for _, a := range c.free {
						if int(a.Index) == idx {
							has = true
						}
					}
					if !has {
						ok = false
					}
				}
				if !ok {
					continue
				}
			}
		}
		candidates = append(candidates, c)
	}

	nodeCount := s.workloadNodeCount(m, w)
	var rankList []int32
	if len(w.Ranks) > 0 {
		for _, r := range w.Ranks {
			rankList = append(rankList, int32(r))
		}
	} else {
		for r := 0; r < nodeCount; r++ {
			rankList = append(rankList, int32(r))
		}
	}
	sort.Slice(rankList, func(i, j int) bool { return rankList[i] < rankList[j] })

	byID := map[string]*candidate{}
	for i := range candidates {
		c := &candidates[i]
		byID[c.nodeID] = c
	}
	nodeStatus := map[string]string{}
	for _, n := range nodes {
		nodeStatus[n.ID] = n.Status
	}
	// Accelerator allocation within this plan: UUIDs per node, plus the
	// set of nodes a devAll rank has consumed (no second rank there).
	allocUUID := map[string]map[string]bool{}
	usedAll := map[string]bool{}
	placements := []Placement{}
	markAlloc := func(nodeID string, uuids []string) {
		if allocUUID[nodeID] == nil {
			allocUUID[nodeID] = map[string]bool{}
		}
		for _, u := range uuids {
			allocUUID[nodeID][u] = true
		}
	}
	freeAfter := func(c *candidate) []inventory.Accelerator {
		taken := allocUUID[c.nodeID]
		if len(taken) == 0 {
			return c.free
		}
		var out []inventory.Accelerator
		for _, a := range c.free {
			if !taken[a.UUID] {
				out = append(out, a)
			}
		}
		return out
	}
	allocOne := func(c *candidate, rank int32) (Placement, error) {
		pl := Placement{NodeID: c.nodeID, NodeName: c.nodeName, Rank: rank}
		if !reqAcc.Required {
			return pl, nil
		}
		avail := freeAfter(c)
		switch {
		case devAll:
			if usedAll[c.nodeID] {
				return pl, fmt.Errorf("node already hosts an all-accelerators rank in this plan")
			}
			if len(avail) == 0 {
				return pl, fmt.Errorf("no free accelerators")
			}
			for _, a := range avail {
				pl.Accelerators = append(pl.Accelerators, a.UUID)
			}
			pl.AcceleratorIndex = avail[0].Index
			pl.AcceleratorUUID = avail[0].UUID
			usedAll[c.nodeID] = true
		case len(devIndices) > 0:
			taken := allocUUID[c.nodeID]
			for _, idx := range devIndices {
				got := false
				for _, a := range c.free {
					if int(a.Index) == idx && !taken[a.UUID] {
						pl.Accelerators = append(pl.Accelerators, a.UUID)
						if !got {
							pl.AcceleratorIndex = int32(idx)
							pl.AcceleratorUUID = a.UUID
							got = true
						}
					}
				}
				if !got {
					return pl, fmt.Errorf("accelerator index %d not free on node", idx)
				}
			}
		default:
			need := reqAcc.Count
			if len(avail) < need {
				return pl, fmt.Errorf("only %d of %d requested accelerators free", len(avail), need)
			}
			for _, a := range avail[:need] {
				pl.Accelerators = append(pl.Accelerators, a.UUID)
			}
			pl.AcceleratorIndex = avail[0].Index
			pl.AcceleratorUUID = avail[0].UUID
		}
		markAlloc(c.nodeID, pl.Accelerators)
		return pl, nil
	}

	if len(req.Placements) > 0 {
		covered := map[int32]bool{}
		for _, ov := range req.Placements {
			if covered[ov.Rank] {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.duplicate_rank",
					fmt.Sprintf("rank %d appears more than once", ov.Rank)))
				continue
			}
			inSet := false
			for _, r := range rankList {
				if r == ov.Rank {
					inSet = true
				}
			}
			if !inSet {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.unknown_rank",
					fmt.Sprintf("rank %d is not in the recipe rank set", ov.Rank)))
				continue
			}
			covered[ov.Rank] = true
			c, okc := byID[ov.NodeID]
			if !okc {
				code, msg := "placement.node_unavailable",
					fmt.Sprintf("node %s is not an eligible candidate", ov.NodeID)
				if st, known := nodeStatus[ov.NodeID]; known && st == "online" {
					// The node is known and online but was filtered out:
					// name the real reason (no free compatible accelerators)
					// instead of the generic ineligibility.
					code, msg = "placement.accelerator_unavailable",
						fmt.Sprintf("node %s cannot host rank %d: no free compatible accelerators", ov.NodeID, ov.Rank)
				}
				plan.Diagnostics = append(plan.Diagnostics, diag.Error(code, msg))
				continue
			}
			pl, err := allocOne(c, ov.Rank)
			if err != nil {
				delete(covered, ov.Rank)
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.accelerator_unavailable",
					fmt.Sprintf("node %s cannot host rank %d: %v", ov.NodeID, ov.Rank, err)))
				continue
			}
			placements = append(placements, pl)
		}
		for _, r := range rankList {
			if !covered[r] {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.rank_missing",
					fmt.Sprintf("no placement override for rank %d", r)))
			}
		}
	} else {
		sorted := append([]candidate{}, candidates...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return len(sorted[i].free) > len(sorted[j].free)
		})
		for _, r := range rankList {
			var best *candidate
			for i := range sorted {
				c := &sorted[i]
				if reqAcc.Required {
					if devAll {
						if usedAll[c.nodeID] {
							continue
						}
					} else if len(freeAfter(c)) < reqAcc.Count {
						continue
					}
				}
				if best == nil {
					best = c
				}
			}
			if best == nil {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.no_capacity",
					fmt.Sprintf("no eligible node for rank %d", r)))
				continue
			}
			pl, err := allocOne(best, r)
			if err != nil {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("placement.accelerator_unavailable",
					fmt.Sprintf("no node can host rank %d: %v", r, err)))
				continue
			}
			placements = append(placements, pl)
		}
	}
	sort.Slice(placements, func(i, j int) bool { return placements[i].Rank < placements[j].Rank })
	plan.Placements = placements
	if len(placements) < len(rankList) {
		plan.Ready = false
	}

	// Fabric.
	if m.HasFabricRequirement() {
		fabs, err := s.q.ListFabrics(ctx)
		if err != nil {
			return nil, err
		}
		nodeSet := map[string]bool{}
		for _, pl := range placements {
			nodeSet[pl.NodeID] = true
		}
		for _, f := range fabs {
			if f.State != "ok" {
				continue
			}
			if m.Compatibility.Fabric != nil && m.Compatibility.Fabric.Transport != "" &&
				f.Transport != m.Compatibility.Fabric.Transport {
				continue
			}
			var members []string
			if merr := json.Unmarshal([]byte(f.Members), &members); merr != nil {
				plan.Diagnostics = append(plan.Diagnostics, diag.Warning("fabric.members_malformed",
					fmt.Sprintf("fabric %s has unparseable members", f.ID)))
				continue
			}
			memberSet := map[string]bool{}
			for _, mem := range members {
				memberSet[mem] = true
			}
			okc := true
			for n := range nodeSet {
				if !memberSet[n] {
					okc = false
				}
			}
			if !okc {
				continue
			}
			plan.Fabric = &f.ID
			break
		}
		if plan.Fabric == nil {
			plan.Diagnostics = append(plan.Diagnostics, diag.Error("fabric.unavailable",
				"no fabric matching the recipe requirement covers the placed nodes"))
		}
	}

	// Ports + endpoint. Per-rank host port = base + rank, base = host || container.
	nodeByID := map[string]*candidate{}
	for i := range candidates {
		nodeByID[candidates[i].nodeID] = &candidates[i]
	}
	if len(w.Ports) > 0 {
		type portKey struct {
			addr string
			port int
		}
		planPorts := map[portKey][]string{} // -> placement node ids
		for _, pl := range placements {
			c := nodeByID[pl.NodeID]
			addr := pl.NodeName
			if c != nil {
				addr = c.addr
			}
			for _, p := range w.Ports {
				base := p.Host
				if base == 0 {
					base = p.Container
				}
				hostPort := base + int(pl.Rank)
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				plan.Ports = append(plan.Ports, PortPreview{
					NodeName:      pl.NodeName,
					HostPort:      int32(hostPort),
					ContainerPort: int32(p.Container),
					Protocol:      proto,
				})
				k := portKey{addr: addr, port: hostPort}
				planPorts[k] = append(planPorts[k], pl.NodeID)
				if leased[fmt.Sprintf("port:%s:%d", pl.NodeID, hostPort)] {
					plan.Conflicts = append(plan.Conflicts, Conflict{
						Resource:   fmt.Sprintf("port:%s:%d", pl.NodeID, hostPort),
						OccupiedBy: "active lease",
					})
				}
			}
		}
		// Active-deployment endpoint conflicts.
		active, err := s.q.ListActiveDeployments(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range active {
			if !d.Endpoint.Valid || d.Endpoint.String == "" {
				continue
			}
			parts := strings.SplitN(d.Endpoint.String, ":", 2)
			if len(parts) != 2 {
				continue
			}
			var otherPort int
			if _, err := fmt.Sscanf(parts[1], "%d", &otherPort); err != nil {
				continue
			}
			if nodeIDs, hit := planPorts[portKey{addr: parts[0], port: otherPort}]; hit {
				for _, nID := range nodeIDs {
					plan.Conflicts = append(plan.Conflicts, Conflict{
						Resource:     fmt.Sprintf("port:%s:%d", nID, otherPort),
						DeploymentID: d.ID,
						OccupiedBy:   d.ID,
					})
				}
			}
		}
		if len(placements) > 0 {
			r0 := placements[0]
			addr := r0.NodeName
			if c := nodeByID[r0.NodeID]; c != nil {
				addr = c.addr
			}
			first := w.Ports[0]
			base := first.Host
			if base == 0 {
				base = first.Container
			}
			ep := Endpoint{
				Host:  addr,
				Port:  int32(base + int(r0.Rank)),
				Model: profileString(values, "model"),
			}
			if w.Readiness != nil && w.Readiness.HTTPGet != nil && w.Readiness.HTTPGet.Path != "" {
				ep.Path = w.Readiness.HTTPGet.Path
			} else if w.Verify != nil && w.Verify.HTTPGet != nil {
				ep.Path = w.Verify.HTTPGet.Path
			}
			plan.Endpoint = ep
		}
	}

	// Artifacts.
	for _, a := range m.Artifacts {
		dest := a.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + a.Name
		}
		art, aerr := s.q.GetArtifactByIdentity(ctx, a.Source.Identity)
		if aerr != nil {
			// Unknown artifact: nothing in the fleet holds it; the library
			// must install it first. Blocking.
			plan.Transfers = append(plan.Transfers, TransferPreview{
				ArtifactID: a.Source.Identity,
				Identity:   a.Source.Identity,
				SourceNode: "origin",
				DestNode:   "all",
				DestPath:   dest,
			})
			plan.Risks = append(plan.Risks, "artifact:"+a.Name+":origin_download")
			plan.Diagnostics = append(plan.Diagnostics, diag.Error("artifact.unplaced",
				"no node holds "+a.Source.Identity+"; install it via the library first"))
			continue
		}
		var missing []string
		for _, pl := range placements {
			placed, perr := s.artifactPlaced(ctx, art.ID, pl.NodeID)
			if perr != nil {
				return nil, perr
			}
			if !placed {
				missing = append(missing, pl.NodeName)
			}
		}
		if len(missing) == 0 {
			continue
		}
		srcName := s.transferSourceName(ctx, art.ID)
		for _, name := range missing {
			plan.Transfers = append(plan.Transfers, TransferPreview{
				ArtifactID: art.ID,
				Identity:   art.Identity,
				SourceNode: srcName,
				DestNode:   name,
				DestPath:   dest,
				Bytes:      artifactSize(art.Metadata),
			})
		}
		if srcName == "" {
			plan.Diagnostics = append(plan.Diagnostics, diag.Error("artifact.unplaced",
				"no node holds a valid copy of "+art.Identity))
		}
	}

	plan.Risks = append(plan.Risks, m.HighRiskPermissions()...)

	plan.Ready = len(placements) == len(rankList) && len(plan.Conflicts) == 0 &&
		!diag.HasError(plan.Diagnostics)
	plan.Digest = plan.PlanDigest()
	return plan, nil
}

func (s *Service) accelRequirement(m *recipe.Manifest) *planAccRequirement {
	req := &planAccRequirement{Count: 1}
	required := false
	if m.Compatibility.Accelerator != nil {
		required = true
		req.Vendor = m.Compatibility.Accelerator.Vendor
		req.Architectures = m.Compatibility.Accelerator.Architectures
		req.MinMemory = m.Compatibility.Accelerator.MinMemoryBytes
		req.Features = m.Compatibility.Accelerator.Features
		if m.Compatibility.Accelerator.Count > 0 {
			req.Count = m.Compatibility.Accelerator.Count
		}
	}
	for _, w := range m.Workloads {
		if w.Devices != nil && w.Devices.Accelerator != nil {
			required = true
		}
	}
	req.Required = required
	return req
}

func (s *Service) workloadNodeCount(m *recipe.Manifest, w *recipe.Workload) int {
	if len(w.Ranks) > 0 {
		return len(w.Ranks)
	}
	if m.Compatibility.NodeCount > 0 {
		return m.Compatibility.NodeCount
	}
	return 1
}

func (s *Service) fleetAccProfile(nodes []db.Node) (string, string, []string) {
	for _, n := range nodes {
		if !n.Inventory.Valid || n.Inventory.String == "" {
			continue
		}
		var inv inventory.Inventory
		if err := json.Unmarshal([]byte(n.Inventory.String), &inv); err != nil {
			continue
		}
		if len(inv.Accelerators) == 0 {
			continue
		}
		a := inv.Accelerators[0]
		return a.Vendor, a.Architecture, a.Features
	}
	return "", "", nil
}

func (s *Service) artifactPlaced(ctx context.Context, artifactID, nodeID string) (bool, error) {
	rows, err := s.q.ListPlacements(ctx, artifactID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.NodeID == nodeID && r.State == "valid" {
			return true, nil
		}
	}
	return false, nil
}

func artifactSize(metadata string) int64 {
	if metadata == "" {
		return 0
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(metadata), &v); err != nil {
		return 0
	}
	switch b := v["size_bytes"].(type) {
	case float64:
		return int64(b)
	case json.Number:
		if i, err := b.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// transferSourceName returns the name of any node holding a valid copy of
// the artifact ("" when none does).
func (s *Service) transferSourceName(ctx context.Context, artifactID string) string {
	rows, err := s.q.ListPlacements(ctx, artifactID)
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if r.State != "valid" {
			continue
		}
		if n, nerr := s.q.GetNode(ctx, r.NodeID); nerr == nil {
			return n.DisplayName
		}
	}
	return ""
}

func profileString(values map[string]any, key string) string {
	if v, ok := values[key]; ok {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return ""
}

// ---------------------------------------------------------------- placement storage

// placementSet is the deployments.placement JSON document.
type placementSet struct {
	Ranks   map[string]int `json:"ranks"`
	Entries []Placement    `json:"entries"`
	// Workload is the manifest workload-variant index the plan selected;
	// render/verify execute exactly this variant. Nil for rows created
	// before the field existed (fleet-based reselection).
	Workload *int `json:"workload,omitempty"`
}

// ParsePlacementSet decodes a stored placement document; it tolerates the
// legacy map[nodeID]rank shape.
func ParsePlacementSet(raw string) placementSet {
	var ps placementSet
	if err := json.Unmarshal([]byte(raw), &ps); err == nil {
		return ps
	}
	var legacy map[string]int
	if err := json.Unmarshal([]byte(raw), &legacy); err == nil {
		ps.Ranks = legacy
	}
	return ps
}

func (ps placementSet) Marshal() string {
	if ps.Ranks == nil {
		ps.Ranks = map[string]int{}
	}
	b, _ := json.Marshal(ps)
	return string(b)
}

// RanksOnNode lists the ranks this node hosts; Entries is authoritative
// (multiple ranks may share one node), the legacy Ranks map is fallback.
func (ps placementSet) RanksOnNode(nodeID string) []int32 {
	var out []int32
	if len(ps.Entries) > 0 {
		for _, e := range ps.Entries {
			if e.NodeID == nodeID {
				out = append(out, e.Rank)
			}
		}
	} else {
		for n, r := range ps.Ranks {
			if n == nodeID {
				out = append(out, int32(r))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (ps placementSet) EntryFor(rank int32) *Placement {
	for i := range ps.Entries {
		if ps.Entries[i].Rank == rank {
			return &ps.Entries[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- create

// Create validates the plan digest, commits the deployment with its exact
// resource leases, and starts the dispatch sequence.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Deployment, error) {
	plan, err := s.Plan(ctx, PlanRequest{
		RecipeDigest: req.RecipeDigest,
		Profile:      req.Profile,
		Placements:   req.Placements,
	})
	if err != nil {
		return nil, err
	}
	if req.PlanDigest != "" && req.PlanDigest != plan.Digest {
		return nil, fmt.Errorf("%w: %s != %s", ErrPlanStale, req.PlanDigest, plan.Digest)
	}
	if !plan.Ready {
		return nil, fmt.Errorf("%w: %v", ErrNotReady, diag.Decode(diag.Encode(plan.Diagnostics)))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	qtx := db.New(tx)

	depID, _ := id.New()
	runIDStr, _ := id.New()
	wi := plan.WorkloadIndex
	ps := placementSet{Ranks: map[string]int{}, Entries: plan.Placements, Workload: &wi}
	for _, pl := range plan.Placements {
		ps.Ranks[pl.NodeID] = int(pl.Rank)
	}
	var fabric sql.NullString
	if plan.Fabric != nil {
		fabric = sql.NullString{String: *plan.Fabric, Valid: true}
	}
	// The deployment row must exist before the run: runs.deployment_id is
	// a foreign key. run_id is linked in a second statement.
	if err := qtx.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:           depID,
		RecipeDigest: plan.RecipeDigest,
		Profile:      plan.Profile,
		Placement:    ps.Marshal(),
		Fabric:       fabric,
	}); err != nil {
		tx.Rollback()
		return nil, err
	}
	input, _ := json.Marshal(map[string]any{
		"recipe_digest": plan.RecipeDigest,
		"profile":       plan.Profile,
		"plan_digest":   plan.Digest,
	})
	if err := qtx.CreateRun(ctx, db.CreateRunParams{
		ID:           runIDStr,
		Module:       "serving",
		Kind:         "serve",
		State:        "queued",
		Resources:    "[]",
		Input:        string(input),
		DeploymentID: sql.NullString{String: depID, Valid: true},
	}); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := qtx.UpdateDeploymentRunID(ctx, db.UpdateDeploymentRunIDParams{
		RunID: sql.NullString{String: runIDStr, Valid: true},
		ID:    depID,
	}); err != nil {
		tx.Rollback()
		return nil, err
	}
	endpoint := ""
	if plan.Endpoint.Host != "" {
		endpoint = fmt.Sprintf("%s:%d", plan.Endpoint.Host, plan.Endpoint.Port)
	}
	if endpoint != "" {
		if err := qtx.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
			ObservedState: "unknown",
			Endpoint:      sql.NullString{String: endpoint, Valid: true},
			ID:            depID,
		}); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	if err := s.runs.AcquireLeases(ctx, qtx, "deployment", depID, plan.LeaseResources()); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, "run.created", runIDStr, mustJSON(map[string]any{"run_id": runIDStr, "module": "serving"}))
	s.bus.Publish(ctx, "deployment.created", depID, mustJSON(map[string]any{
		"deployment_id": depID, "run_id": runIDStr, "recipe": plan.RecipeName,
	}))
	_ = s.runs.SetState(ctx, runIDStr, runs.Planning, "", "")
	for _, pl := range plan.Placements {
		s.dispatchNext(ctx, depID, pl.Rank, runIDStr, pl)
	}
	_ = s.runs.SetState(ctx, runIDStr, runs.Running, "", "")

	return s.Get(ctx, depID)
}

// ---------------------------------------------------------------- dispatch

func (s *Service) inflightMark(commandID, depID string, rank int32, op string) {
	s.mu.Lock()
	s.inflight[commandID] = &inflightCmd{DepID: depID, Rank: rank, Op: op}
	s.mu.Unlock()
}

func (s *Service) inflightTake(commandID string) (*inflightCmd, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.inflight[commandID]
	delete(s.inflight, commandID)
	return c, ok
}

func (s *Service) sendWorkload(nodeID, cmdID string, op agentv1.WorkloadOp, depID, runID string, rank int32, spec *runtime.ContainerSpec) bool {
	var specBytes []byte
	if spec != nil {
		if b, err := json.Marshal(spec); err == nil {
			specBytes = b
		}
	}
	return s.nodes.Send(nodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_WorkloadCommand{
		WorkloadCommand: &agentv1.WorkloadCommand{
			CommandId:     cmdID,
			Op:            op,
			DeploymentId:  depID,
			RunId:         runID,
			Rank:          rank,
			ContainerSpec: specBytes,
		},
	}})
}

// dispatchNext sends the next workload operation for one rank according to
// its persisted phase and the deployment's desired state. Completed phases
// advance only on success ack (OnCommandResult) or agent state report —
// a restart before the ack re-drives the same operation, which every op
// tolerates: PULL is a no-op on existing images, CREATE and START tolerate
// "exists"/"already running", STOP is idempotent.
//
//	desired=running:
//	  none    -> artifact gate, then PULL
//	  pulled  -> CREATE
//	  created -> START
//	  started -> INSPECT (running confirmation)
//	desired=stopped:
//	  started  -> STOP, then phase stopping (pending marker)
//	  stopping -> STOP re-drive
//	  stopped  -> nothing
func (s *Service) dispatchNext(ctx context.Context, depID string, rank int32, runID string, pl Placement) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	ph := ParseDispatch(row.Dispatch)
	phase := ph.Get(rank)
	nodeID := pl.NodeID

	switch row.DesiredState {
	case "running":
		switch phase {
		case PhaseNone:
			// Container dispatch is gated on every recipe artifact having a
			// valid placement on this rank's node; missing copies are
			// filled by peer transfer before PULL.
			if !s.ensureArtifacts(ctx, row, rank, pl) {
				return
			}
			spec, err := s.renderSpec(ctx, depID, rank, runID, &pl)
			if err != nil {
				s.noteDispatch(ctx, depID, diag.Error("artifact.mount_missing", err.Error()))
				return
			}
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "pull")
			if !s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_PULL, depID, runID, rank, spec) {
				s.noteDispatch(ctx, depID, diag.Error("workload.node_offline",
					fmt.Sprintf("node %s offline; dispatch paused until reconnect", nodeID)))
				return
			}
		case PhasePulled:
			spec, err := s.renderSpec(ctx, depID, rank, runID, &pl)
			if err != nil {
				s.noteDispatch(ctx, depID, diag.Error("artifact.mount_missing", err.Error()))
				return
			}
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "create")
			_ = s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, depID, runID, rank, spec)
		case PhaseCreated:
			// START resolves the container by name; the agent keeps the
			// CREATE spec in memory, so no re-render is needed (and a
			// placement failure must not block a stop).
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "start")
			_ = s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_START, depID, runID, rank, nil)
		case PhaseStarted:
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "inspect")
			_ = s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, depID, runID, rank, nil)
		}
	case "stopped":
		switch phase {
		case PhaseNone:
			// PULL never acked: CREATE/START never sent, no container can
			// exist. Confirm stopped directly.
			s.setPhase(ctx, depID, rank, PhaseStopped)
			s.checkStopComplete(ctx, depID, runID)
		case PhasePulled, PhaseCreated, PhaseStarted, PhaseStopping:
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "stop")
			if s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_STOP, depID, runID, rank, nil) &&
				phase == PhaseStarted {
				s.setPhase(ctx, depID, rank, PhaseStopping)
			}
		}
	}
}

func (s *Service) setPhase(ctx context.Context, depID string, rank int32, to string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	ph := ParseDispatch(row.Dispatch)
	ph[rank] = to
	b, _ := json.Marshal(ph)
	_ = s.q.SetDeploymentDispatch(ctx, db.SetDeploymentDispatchParams{Dispatch: string(b), ID: depID})
}

// OnCommandResult advances the dispatch state machine for one rank.
func (s *Service) OnCommandResult(ctx context.Context, cr *agentv1.CommandResult) {
	c, ok := s.inflightTake(cr.CommandId)
	if !ok {
		return // not ours (library/peers) or already processed
	}
	depID, rank := c.DepID, c.Rank
	runID := s.runIDFor(ctx, depID)
	pl := s.placementFor(ctx, depID, rank)
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	desired := row.DesiredState
	ph := ParseDispatch(row.Dispatch)
	if ph.Get(rank) == PhaseStopped && desired == "stopped" {
		return // late ack after stop already confirmed
	}

	switch c.Op {
	case "pull":
		if !cr.Ok {
			if desired == "stopped" {
				// No container can exist (CREATE only follows a PULL ack).
				s.setPhase(ctx, depID, rank, PhaseStopped)
				s.checkStopComplete(ctx, depID, runID)
				return
			}
			s.failDispatch(ctx, depID, rank, runID, "workload.pull_failed", cr.Error)
			return
		}
		if desired == "stopped" {
			s.setPhase(ctx, depID, rank, PhaseStopped)
			s.checkStopComplete(ctx, depID, runID)
			return
		}
		s.setPhase(ctx, depID, rank, PhasePulled)
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "create":
		exists := isContainerExists(cr.Error)
		if !cr.Ok && !exists {
			if desired == "stopped" {
				// Container never materialized: nothing to stop.
				s.setPhase(ctx, depID, rank, PhaseStopped)
				s.checkStopComplete(ctx, depID, runID)
				return
			}
			s.failDispatch(ctx, depID, rank, runID, "workload.create_failed", cr.Error)
			return
		}
		if desired == "stopped" {
			// Container now exists (created or stopped): drive it to stopped.
			s.setPhase(ctx, depID, rank, PhaseStopping)
			s.dispatchNext(ctx, depID, rank, runID, pl)
			return
		}
		s.setPhase(ctx, depID, rank, PhaseCreated)
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "start":
		if !cr.Ok && !isContainerExists(cr.Error) && !isAlreadyRunning(cr.Error) {
			if desired == "stopped" {
				// Container exists but did not start: STOP still applies.
				s.setPhase(ctx, depID, rank, PhaseStopping)
				s.dispatchNext(ctx, depID, rank, runID, pl)
				return
			}
			s.failDispatch(ctx, depID, rank, runID, "workload.start_failed", cr.Error)
			return
		}
		if desired == "stopped" {
			s.setPhase(ctx, depID, rank, PhaseStopping)
			s.dispatchNext(ctx, depID, rank, runID, pl)
			return
		}
		s.setPhase(ctx, depID, rank, PhaseStarted)
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "inspect":
		if !cr.Ok && isContainerMissing(cr.Error) && desired == "running" {
			// Container vanished while desired=running: restart the
			// full sequence for this rank.
			s.setPhase(ctx, depID, rank, PhaseNone)
			s.dispatchNext(ctx, depID, rank, runID, pl)
		}
	case "stop":
		missing := !cr.Ok && isContainerMissing(cr.Error)
		if cr.Ok || missing {
			s.setPhase(ctx, depID, rank, PhaseStopped)
			s.checkStopComplete(ctx, depID, runID)
		}
	}
}

func (s *Service) placementFor(ctx context.Context, depID string, rank int32) Placement {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return Placement{}
	}
	if e := ParsePlacementSet(row.Placement).EntryFor(rank); e != nil {
		return *e
	}
	return Placement{}
}

func (s *Service) runIDFor(ctx context.Context, depID string) string {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil || !row.RunID.Valid {
		return ""
	}
	return row.RunID.String
}

func isContainerExists(msg string) bool {
	return strings.Contains(msg, "exists")
}

func isAlreadyRunning(msg string) bool {
	return strings.Contains(msg, "already running") || strings.Contains(msg, "already started")
}

func isContainerMissing(msg string) bool {
	return strings.Contains(msg, "missing") || strings.Contains(msg, "no such")
}

// ---------------------------------------------------------------- artifact transfers

// transferCred mirrors the agent's peer-transfer credential (same JSON
// shape); the controller CA signs canonicalJSON().
type transferCred struct {
	Role         string `json:"role"`
	NodeID       string `json:"node_id"`
	ArtifactID   string `json:"artifact_id"`
	SrcPath      string `json:"src_path"`
	ExpUnix      int64  `json:"exp_unix"`
	SourceSha256 string `json:"source_sha256"`
	SrcSize      int64  `json:"src_size"`
	DestSha256   string `json:"dest_sha256"`
	PeerAddr     string `json:"peer_addr"`
	DestPath     string `json:"dest_path"`
	Signature    string `json:"signature"`
}

func (c *transferCred) canonicalJSON() []byte {
	cp := *c
	cp.Signature = ""
	data, _ := json.Marshal(&cp)
	return data
}

// parseTransferKey splits "depID|nodeID|artifactIdentity" (identities and
// node/deployment ids contain no "|").
func parseTransferKey(key string) (depID, nodeID, artifactID string, ok bool) {
	depID, rest, f1 := strings.Cut(key, "|")
	nodeID, artifactID, f2 := strings.Cut(rest, "|")
	return depID, nodeID, artifactID, f1 && f2
}

// validPlacement returns the stored path of a valid placement of the
// artifact on the node.
func (s *Service) validPlacement(ctx context.Context, artifactID, nodeID string) (string, bool) {
	rows, err := s.q.ListPlacements(ctx, artifactID)
	if err != nil {
		return "", false
	}
	for _, r := range rows {
		if r.NodeID == nodeID && r.State == "valid" {
			return r.Path, true
		}
	}
	return "", false
}

// ensureArtifacts gates container dispatch on every recipe artifact having
// a valid placement on this rank's node. Missing copies are filled with a
// peer transfer from a node that already holds one. Returns true when all
// artifacts are placed; false when a transfer is in flight (placement and
// ack events re-drive dispatch) or the deployment was failed.
func (s *Service) ensureArtifacts(ctx context.Context, row db.GetDeploymentRow, rank int32, pl Placement) bool {
	m, err := s.manifestFor(ctx, row.RecipeDigest)
	if err != nil {
		s.noteDispatch(ctx, row.ID, diag.Error("recipe.manifest", err.Error()))
		return false
	}
	runID := s.runIDFor(ctx, row.ID)
	for _, a := range m.Artifacts {
		art, err := s.q.GetArtifactByIdentity(ctx, a.Source.Identity)
		if err != nil {
			s.failDispatch(ctx, row.ID, rank, runID, "artifact.unplaced",
				"no node holds "+a.Source.Identity)
			return false
		}
		if _, ok := s.validPlacement(ctx, art.ID, pl.NodeID); ok {
			continue
		}
		key := row.ID + "|" + pl.NodeID + "|" + art.Identity
		s.mu.Lock()
		if s.transferInflight[key] != "" {
			s.mu.Unlock()
			return false // transfer already in flight
		}
		tid, derr := s.startTransfer(ctx, art, "", pl.NodeID, "", row.Fabric.String)
		if derr != nil {
			s.mu.Unlock()
			s.failDispatch(ctx, row.ID, rank, runID, "artifact.transfer_failed", derr.Error())
			return false
		}
		s.transferInflight[key] = tid
		s.mu.Unlock()
		s.bus.Publish(ctx, "transfer.started", pl.NodeID, mustJSON(map[string]any{
			"transfer_id": tid, "artifact": art.Identity, "dest_node": pl.NodeID,
		}))
		return false
	}
	return true
}

// StartTransfer plans and dispatches a peer transfer of art from an
// explicit source node to destNodeID at destPath. destPath must be a safe
// relative path (nested paths preserved; absolute or ".." rejected); an
// empty destPath derives it from the source placement.
func (s *Service) StartTransfer(ctx context.Context, art db.Artifact, sourceNodeID, destNodeID, destPath string) (string, error) {
	return s.startTransfer(ctx, art, sourceNodeID, destNodeID, destPath, "")
}

// safeRelPath normalizes a requested destination path: relative, slash-
// separated, no ".." components; the base name survives for a bare file.
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	p = path.Clean(strings.TrimSpace(p))
	if p == "." {
		return "", errors.New("dest_path must name a file or directory")
	}
	if strings.Contains(p, "\\") {
		return "", errors.New("dest_path must be slash-separated")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errors.New("dest_path may not contain ..")
		}
	}
	return p, nil
}

// startTransfer records a transfers row, signs the peer credential, and
// sends the TransferCommand to source (streams) and destination (listener).
// An empty sourceNodeID selects any online node holding a valid placement
// (fabric-prefixed); a non-empty sourceNodeID requires that exact node.
func (s *Service) startTransfer(ctx context.Context, art db.Artifact, sourceNodeID, destNodeID, destPath, fabric string) (string, error) {
	destRel, err := safeRelPath(destPath)
	if err != nil {
		return "", err
	}
	dest, err := s.q.GetNode(ctx, destNodeID)
	if err != nil {
		return "", err
	}
	peerAddr := ""
	if dest.Inventory.Valid {
		if inv, perr := inventory.Parse(dest.Inventory.String); perr == nil {
			peerAddr = inv.PeerListen
		}
	}
	if peerAddr == "" {
		return "", fmt.Errorf("node %s advertises no peer address (set LMW_PEER_ADVERTISE)", destNodeID)
	}
	rows, err := s.q.ListPlacements(ctx, art.ID)
	if err != nil {
		return "", err
	}
	type cand struct {
		pl   db.ArtifactPlacement
		node db.Node
	}
	var src cand
	haveSrc := false
	if sourceNodeID != "" {
		if sourceNodeID == destNodeID {
			return "", errors.New("source and dest node must differ")
		}
		plrows, perr := s.q.ListPlacementsOnNode(ctx, sourceNodeID)
		if perr != nil {
			return "", perr
		}
		var pl db.ArtifactPlacement
		found := false
		for _, p := range plrows {
			if p.ArtifactID == art.ID && p.State == "valid" {
				pl, found = p, true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("node %s holds no valid copy of %s", sourceNodeID, art.Identity)
		}
		n, nerr := s.q.GetNode(ctx, sourceNodeID)
		if nerr != nil {
			return "", nerr
		}
		if !s.nodes.Online(sourceNodeID) {
			return "", fmt.Errorf("source node %s is offline", sourceNodeID)
		}
		src, haveSrc = cand{pl: pl, node: n}, true
	} else {
		var cands []cand
		for _, p := range rows {
			if p.State != "valid" || p.NodeID == destNodeID {
				continue
			}
			n, nerr := s.q.GetNode(ctx, p.NodeID)
			if nerr != nil || !s.nodes.Online(n.ID) {
				continue
			}
			cands = append(cands, cand{pl: p, node: n})
		}
		if len(cands) == 0 {
			return "", errors.New("no online node holds a valid copy of " + art.Identity)
		}
		// Prefer a node in the deployment's fabric (p2p path), else any source.
		src, haveSrc = cands[0], true
		if fabric != "" {
			if f, ferr := s.q.GetFabric(ctx, fabric); ferr == nil {
				var members []string
				_ = json.Unmarshal([]byte(f.Members), &members)
				for _, c := range cands {
					for _, m := range members {
						if m == c.node.ID {
							src = c
							break
						}
					}
				}
			}
		}
	}
	if !haveSrc {
		return "", errors.New("no source placement available")
	}
	tid, _ := id.New()
	if destRel == "" {
		destRel = path.Base(src.pl.Path)
	}
	cred := &transferCred{
		Role:       "source",
		NodeID:     src.node.ID,
		ArtifactID: art.Identity,
		SrcPath:    src.pl.Path,
		ExpUnix:    time.Now().Add(time.Hour).Unix(),
		SrcSize:    src.pl.SizeBytes,
		PeerAddr:   peerAddr,
		DestPath:   destRel,
	}
	sig, err := s.ca.SignCA(cred.canonicalJSON())
	if err != nil {
		return "", fmt.Errorf("sign transfer credential: %w", err)
	}
	cred.Signature = sig
	credJ, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(credJ)
	if err := s.q.CreateTransfer(ctx, db.CreateTransferParams{
		ID:             tid,
		ArtifactID:     art.ID,
		SourceNode:     src.node.ID,
		DestNode:       destNodeID,
		DestPath:       destRel,
		CredentialHash: sum[:],
	}); err != nil {
		return "", err
	}
	tc := &agentv1.TransferCommand{
		TransferId:       tid,
		Credential:       base64.StdEncoding.EncodeToString(credJ),
		ArtifactIdentity: art.Identity,
		SrcPath:          src.pl.Path,
		DestPath:         destRel,
		TimeoutSeconds:   3600,
	}
	// Destination first so its session context is current, then source.
	s.nodes.Send(destNodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{
		TransferCommand: &agentv1.TransferCommand{
			TransferId: tc.TransferId, Role: "dest", PeerAddress: peerAddr,
			Credential: tc.Credential, ArtifactIdentity: tc.ArtifactIdentity,
			SrcPath: tc.SrcPath, DestPath: tc.DestPath, TimeoutSeconds: tc.TimeoutSeconds,
		},
	}})
	if !s.nodes.Send(src.node.ID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{
		TransferCommand: &agentv1.TransferCommand{
			TransferId: tc.TransferId, Role: "source", PeerAddress: peerAddr,
			Credential: tc.Credential, ArtifactIdentity: tc.ArtifactIdentity,
			SrcPath: tc.SrcPath, DestPath: tc.DestPath, TimeoutSeconds: tc.TimeoutSeconds,
		},
	}}) {
		_ = s.q.UpdateTransferState(ctx, db.UpdateTransferStateParams{
			State:      "failed",
			Diagnostic: sql.NullString{String: "source node offline", Valid: true},
			ID:         tid,
		})
		return "", errors.New("source node offline")
	}
	return tid, nil
}

// OnPlacementReport is invoked by the server on every node placement report.
// A valid report unblocks the gated dispatch; an invalid report fails the
// affected ranks.
func (s *Service) OnPlacementReport(ctx context.Context, nodeID, artifactID, state string) {
	type wake struct {
		depID string
		rank  int32
	}
	var wakes []wake
	s.mu.Lock()
	for key, tid := range s.transferInflight {
		depID, plNode, art, ok := parseTransferKey(key)
		if !ok || art != artifactID || plNode != nodeID {
			continue
		}
		delete(s.transferInflight, key)
		tState, tDiag := "succeeded", sql.NullString{}
		if state != "valid" {
			tState = "failed"
			tDiag = sql.NullString{String: "destination reported invalid placement", Valid: true}
		}
		_ = s.q.UpdateTransferState(ctx, db.UpdateTransferStateParams{
			State: tState, Diagnostic: tDiag, ID: tid,
		})
		row, err := s.q.GetDeployment(ctx, depID)
		if err != nil {
			continue
		}
		for _, r := range ParsePlacementSet(row.Placement).RanksOnNode(nodeID) {
			wakes = append(wakes, wake{depID: depID, rank: r})
			if state != "valid" {
				s.failDispatch(ctx, depID, r, s.runIDFor(ctx, depID),
					"artifact.invalid_placement", artifactID+" reported invalid on "+nodeID)
			}
		}
	}
	s.mu.Unlock()
	if state != "valid" {
		return
	}
	for _, w := range wakes {
		runID := s.runIDFor(ctx, w.depID)
		s.dispatchNext(ctx, w.depID, w.rank, runID, s.placementFor(ctx, w.depID, w.rank))
	}
}

// OnTransferResult is invoked by the server when a transfer command acks
// with a failure status (the destination's valid placement report is the
// success signal).
func (s *Service) OnTransferResult(ctx context.Context, transferID, msg string) {
	var fails []struct {
		depID string
		rank  int32
	}
	s.mu.Lock()
	for key, tid := range s.transferInflight {
		if tid != transferID {
			continue
		}
		depID, plNode, _, _ := parseTransferKey(key)
		delete(s.transferInflight, key)
		_ = s.q.UpdateTransferState(ctx, db.UpdateTransferStateParams{
			State:      "failed",
			Diagnostic: sql.NullString{String: msg, Valid: msg != ""},
			ID:         tid,
		})
		row, err := s.q.GetDeployment(ctx, depID)
		if err != nil {
			continue
		}
		for _, r := range ParsePlacementSet(row.Placement).RanksOnNode(plNode) {
			fails = append(fails, struct {
				depID string
				rank  int32
			}{depID, r})
		}
	}
	s.mu.Unlock()
	for _, f := range fails {
		s.failDispatch(ctx, f.depID, f.rank, s.runIDFor(ctx, f.depID),
			"artifact.transfer_failed", msg)
	}
}

func (s *Service) failDispatch(ctx context.Context, depID string, rank int32, runID, code, message string) {
	d := diag.Error(code, fmt.Sprintf("rank %d: %s", rank, message)).Res(depID)
	s.noteDispatch(ctx, depID, d)
	if runID != "" {
		_ = s.runs.SetState(ctx, runID, runs.Failed, code, message)
		s.releaseIfTerminal(ctx, depID, runID)
	}
}

func (s *Service) noteDispatch(ctx context.Context, depID string, d diag.Diagnostic) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	existing := diag.Decode(row.Diagnostics)
	existing = append(existing, d)
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState: row.ObservedState,
		Diagnostics:   diag.Encode(existing),
		ID:            depID,
	})
}

// releaseIfTerminal frees the deployment's leases once its run is terminal:
// the resources are physically free again.
func (s *Service) releaseIfTerminal(ctx context.Context, depID, runID string) {
	if runID == "" {
		return
	}
	run, err := s.runs.Get(ctx, runID)
	if err != nil || !runs.State(run.State).Terminal() {
		return
	}
	_ = s.runs.ReleaseLeasesFor(ctx, "deployment", depID)
}

// Converge re-drives every unresolved deployment on a node after
// (re)connect: running deployments resume from the persisted phase
// (started ranks get an INSPECT confirmation); stopping deployments
// re-send STOP for pending ranks. Leases stay held until confirmation.
func (s *Service) Converge(ctx context.Context, nodeID string) {
	all, err := s.q.ListDeployments(ctx)
	if err != nil {
		return
	}
	for _, d := range all {
		if d.DesiredState != "running" && d.DesiredState != "stopped" {
			continue
		}
		if d.ObservedState == "stopped" && d.DesiredState == "stopped" {
			continue
		}
		ps := ParsePlacementSet(d.Placement)
		ranks := ps.RanksOnNode(nodeID)
		if len(ranks) == 0 {
			continue
		}
		runID := ""
		if d.RunID.Valid {
			runID = d.RunID.String
		}
		for _, rank := range ranks {
			if e := ps.EntryFor(rank); e != nil {
				s.dispatchNext(ctx, d.ID, rank, runID, *e)
			}
		}
	}
}

// ---------------------------------------------------------------- state updates

// mapObserved translates agent container states to deployment observed states.
func mapObserved(state, diagnostic string) (observed string, d diag.Diagnostic) {
	switch state {
	case "running":
		if diagnostic != "" {
			return "degraded", diag.Error("workload.degraded", diagnostic)
		}
		return "healthy", diag.Info("workload.healthy", "container running")
	case "paused":
		return "degraded", diag.Error("workload.paused", "container paused")
	case "exited":
		return "stopped", diag.Error("workload.exited", "container exited")
	case "restarting":
		return "starting", diag.Info("workload.restarting", "container restarting")
	case "created":
		return "preparing", diag.Info("workload.created", "container created, awaiting start")
	case "removing":
		return "stopping", diag.Info("workload.removing", "container removing")
	case "dead":
		return "failed", diag.Error("workload.dead", "container in dead state")
	case "missing":
		return "stopped", diag.Error("workload.missing", "container no longer exists")
	default:
		return "unknown", diag.Info("workload.unknown_state", state)
	}
}

// OnStateUpdate applies one agent state report to its deployment.
func (s *Service) OnStateUpdate(ctx context.Context, nodeID string, su *agentv1.StateUpdate) {
	if su.DeploymentId == "" {
		return
	}
	row, err := s.q.GetDeployment(ctx, su.DeploymentId)
	if err != nil {
		return
	}
	observed, d := mapObserved(su.State, su.DiagnosticMessage)
	existing := diag.Decode(row.Diagnostics)
	existing = append(existing, d)
	endpoint := row.Endpoint
	if su.EndpointPort != 0 && su.Rank == 0 {
		if n, nerr := s.q.GetNode(ctx, nodeID); nerr == nil && n.Inventory.Valid {
			var inv inventory.Inventory
			if json.Unmarshal([]byte(n.Inventory.String), &inv) == nil {
				if addr := firstNonLoopback(&inv); addr != "" {
					endpoint = sql.NullString{String: fmt.Sprintf("%s:%d", addr, su.EndpointPort), Valid: true}
				}
			}
		}
	}
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState:     observed,
		Endpoint:          endpoint,
		ModelCapabilities: row.ModelCapabilities,
		Diagnostics:       diag.Encode(existing),
		ID:                su.DeploymentId,
	})
	s.bus.Publish(ctx, "deployment.state", su.DeploymentId, mustJSON(map[string]any{
		"deployment_id": su.DeploymentId, "state": observed, "rank": su.Rank,
	}))

	runID := ""
	if row.RunID.Valid {
		runID = row.RunID.String
	}

	switch row.DesiredState {
	case "stopped":
		// Stop confirmation: the rank is done when the agent reports the
		// container no longer running, or the STOP ack landed (OnCommandResult).
		if su.State == "exited" || su.State == "dead" || su.State == "missing" {
			s.setPhase(ctx, row.ID, su.Rank, PhaseStopped)
			s.checkStopComplete(ctx, row.ID, runID)
		} else if su.State == "running" {
			// STOP has not taken effect yet (reconnect race): re-drive.
			s.setPhase(ctx, row.ID, su.Rank, PhaseStopping)
			s.dispatchNext(ctx, row.ID, su.Rank, runID, s.placementFor(ctx, row.ID, su.Rank))
		}
	case "running":
		if observed == "stopped" || observed == "failed" {
			run, err := s.runs.Get(ctx, runID)
			if err == nil && !runs.State(run.State).Terminal() {
				_ = s.runs.SetState(ctx, runID, runs.Failed, "workload.exited",
					fmt.Sprintf("rank %d: container %s", su.Rank, su.State))
				_ = s.runs.ReleaseLeasesFor(ctx, "deployment", row.ID)
			}
		}
	}
}

// checkStopComplete finalizes a stop once every rank is confirmed stopped.
func (s *Service) checkStopComplete(ctx context.Context, depID, runID string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	ps := ParsePlacementSet(row.Placement)
	ph := ParseDispatch(row.Dispatch)
	for _, rank := range ps.AllRanks() {
		if ph.Get(rank) != PhaseStopped {
			return // one rank not confirmed: keep the lease, stay stopping
		}
	}
	if runID != "" {
		_ = s.runs.SetState(ctx, runID, runs.Cancelled, "", "")
	}
	_ = s.runs.ReleaseLeasesFor(ctx, "deployment", depID)
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState: "stopped",
		ID:            depID,
	})
	s.bus.Publish(ctx, "deployment.stopped", depID, mustJSON(map[string]any{"deployment_id": depID}))
}

// ---------------------------------------------------------------- stop

// Stop issues STOP per rank (online nodes only) and cancels the run.
// Offline ranks keep their leases and are re-driven on reconnect; the
// deployment stays `stopping` until every rank confirms.
func (s *Service) Stop(ctx context.Context, depID string) (*Deployment, error) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, ErrUnknown
	}
	if row.DesiredState != "running" {
		return nil, fmt.Errorf("%w: deployment is %s", ErrState, row.DesiredState)
	}
	_ = s.q.UpdateDeploymentState(ctx, db.UpdateDeploymentStateParams{
		DesiredState: "stopped", ID: depID,
	})
	if row.RunID.Valid {
		_ = s.runs.Cancel(ctx, row.RunID.String)
	}
	runID := ""
	if row.RunID.Valid {
		runID = row.RunID.String
	}
	ps := ParsePlacementSet(row.Placement)
	for _, rank := range ps.AllRanks() {
		if e := ps.EntryFor(rank); e != nil {
			s.dispatchNext(ctx, depID, rank, runID, *e)
		}
	}
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState: "stopping",
		ID:            depID,
	})
	s.bus.Publish(ctx, "deployment.stopping", depID, mustJSON(map[string]any{"deployment_id": depID}))
	return s.Get(ctx, depID)
}

// ---------------------------------------------------------------- verify

// Verify runs the workload probe against the live endpoint.
func (s *Service) Verify(ctx context.Context, depID string) (*Deployment, error) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, ErrUnknown
	}
	if row.ObservedState != "healthy" {
		return nil, fmt.Errorf("%w: observed state is %s", ErrState, row.ObservedState)
	}
	m, err := s.manifestFor(ctx, row.RecipeDigest)
	if err != nil {
		return nil, err
	}
	_, w, err := s.selectWorkload(ctx, row, m)
	if err != nil {
		return nil, err
	}
	probe := w.Verify
	if probe == nil {
		probe = w.Readiness
	}
	if probe == nil || probe.HTTPGet == nil {
		return nil, fmt.Errorf("%w: recipe defines no HTTP probe", ErrState)
	}
	if row.Endpoint.String == "" {
		return nil, fmt.Errorf("%w: no endpoint recorded", ErrState)
	}
	hostPort := row.Endpoint.String
	if probe.HTTPGet.Port != 0 {
		parts := strings.SplitN(hostPort, ":", 2)
		if len(parts) == 2 {
			hostPort = fmt.Sprintf("%s:%d", parts[0], probe.HTTPGet.Port)
		}
	}
	path := probe.HTTPGet.Path
	if path == "" {
		path = "/health"
	}
	values, _ := m.ProfileValues(row.Profile)
	rctx := recipe.RenderContext{
		NodeID:    depID,
		Artifacts: map[string]string{},
		Profiles:  values,
	}
	if strings.Contains(path, "{") {
		if rendered, rerr := m.Render(path, rctx); rerr == nil {
			path = rendered
		}
	}
	timeout := time.Duration(probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := probe.HTTPGet.Method
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(pctx, method, "http://"+hostPort+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.setVerifyResult(ctx, depID, false, err.Error(), "")
		return s.Get(ctx, depID)
	}
	defer resp.Body.Close()
	var body []byte
	buf := make([]byte, 8192)
	for len(body) < 1<<20 {
		n, rerr := resp.Body.Read(buf)
		if rerr != nil {
			break
		}
		body = append(body, buf[:n]...)
	}
	ok := true
	var detail string
	if probe.Expect != nil && probe.Expect.StatusCode != nil && resp.StatusCode != *probe.Expect.StatusCode {
		ok = false
		detail = fmt.Sprintf("expected status %d, got %d", *probe.Expect.StatusCode, resp.StatusCode)
	}
	if ok && probe.Expect != nil && probe.Expect.BodyContains != "" &&
		!strings.Contains(string(body), probe.Expect.BodyContains) {
		ok = false
		detail = "response body does not contain expected content"
	}
	capabilities := ""
	if ok && probe.Expect != nil && len(probe.Expect.JSON) > 0 {
		var parsed map[string]any
		if jerr := json.Unmarshal(body, &parsed); jerr == nil {
			capabilities = string(body)
		}
	}
	if !ok {
		detail = fmt.Sprintf("probe %s: %s", path, detail)
	}
	s.setVerifyResult(ctx, depID, ok, detail, capabilities)
	return s.Get(ctx, depID)
}

func (s *Service) setVerifyResult(ctx context.Context, depID string, ok bool, detail, capabilities string) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return
	}
	var d diag.Diagnostic
	if ok {
		d = diag.Info("verify.passed", "probe ok")
	} else {
		d = diag.Error("verify.failed", detail)
	}
	existing := diag.Decode(row.Diagnostics)
	existing = append(existing, d)
	caps := row.ModelCapabilities
	if capabilities != "" {
		caps = sql.NullString{String: capabilities, Valid: true}
	}
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState:     row.ObservedState,
		Diagnostics:       diag.Encode(existing),
		ModelCapabilities: caps,
		ID:                depID,
	})
	s.bus.Publish(ctx, "deployment.verified", depID, mustJSON(map[string]any{
		"deployment_id": depID, "ok": ok,
	}))
}

// ---------------------------------------------------------------- views

// Deployment is the API view (openapi Deployment).
type Deployment struct {
	ID                string            `json:"id"`
	RecipeDigest      string            `json:"recipe_digest"`
	RecipeName        string            `json:"recipe_name,omitempty"`
	RecipeVersion     string            `json:"recipe_version,omitempty"`
	Profile           string            `json:"profile"`
	Placements        []Placement       `json:"placements"`
	Fabric            *string           `json:"fabric,omitempty"`
	DesiredState      string            `json:"desired_state"`
	ObservedState     string            `json:"observed_state"`
	Endpoint          *Endpoint         `json:"endpoint,omitempty"`
	ModelCapabilities string            `json:"model_capabilities,omitempty"`
	Diagnostics       []diag.Diagnostic `json:"diagnostics,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`
}

// Get returns one deployment view.
func (s *Service) Get(ctx context.Context, depID string) (*Deployment, error) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, ErrUnknown
	}
	return s.view(ctx, rowToDeployment(row))
}

// List returns all deployment views.
func (s *Service) List(ctx context.Context) ([]Deployment, error) {
	rows, err := s.q.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Deployment, 0, len(rows))
	for _, r := range rows {
		if v, err := s.view(ctx, listRowToDeployment(r)); err == nil {
			out = append(out, *v)
		}
	}
	return out, nil
}

// rowToDeployment projects sqlc deployment rows onto the canonical model.
func rowToDeployment(r db.GetDeploymentRow) db.Deployment {
	return db.Deployment{
		ID:                r.ID,
		RecipeDigest:      r.RecipeDigest,
		Profile:           r.Profile,
		Placement:         r.Placement,
		Fabric:            r.Fabric,
		DesiredState:      r.DesiredState,
		ObservedState:     r.ObservedState,
		Endpoint:          r.Endpoint,
		ModelCapabilities: r.ModelCapabilities,
		Diagnostics:       r.Diagnostics,
		RunID:             r.RunID,
		Dispatch:          r.Dispatch,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func listRowToDeployment(r db.ListDeploymentsRow) db.Deployment {
	return db.Deployment{
		ID:                r.ID,
		RecipeDigest:      r.RecipeDigest,
		Profile:           r.Profile,
		Placement:         r.Placement,
		Fabric:            r.Fabric,
		DesiredState:      r.DesiredState,
		ObservedState:     r.ObservedState,
		Endpoint:          r.Endpoint,
		ModelCapabilities: r.ModelCapabilities,
		Diagnostics:       r.Diagnostics,
		RunID:             r.RunID,
		Dispatch:          r.Dispatch,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func (s *Service) view(ctx context.Context, row db.Deployment) (*Deployment, error) {
	v := &Deployment{
		ID:            row.ID,
		RecipeDigest:  row.RecipeDigest,
		Profile:       row.Profile,
		Placements:    ParsePlacementSet(row.Placement).Entries,
		DesiredState:  row.DesiredState,
		ObservedState: row.ObservedState,
		Diagnostics:   diag.Decode(row.Diagnostics),
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	if recipeRow, err := s.q.GetRecipe(ctx, row.RecipeDigest); err == nil {
		v.RecipeName = recipeRow.Name
		v.RecipeVersion = recipeRow.Version
	}
	if row.Fabric.Valid {
		v.Fabric = &row.Fabric.String
	}
	if row.RunID.Valid {
		v.RunID = row.RunID.String
	}
	if row.ModelCapabilities.Valid {
		v.ModelCapabilities = row.ModelCapabilities.String
	}
	if row.Endpoint.Valid && row.Endpoint.String != "" {
		parts := strings.SplitN(row.Endpoint.String, ":", 2)
		if len(parts) == 2 {
			var port int32
			if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil {
				v.Endpoint = &Endpoint{Host: parts[0], Port: port}
			}
		}
	}
	return v, nil
}

// selectWorkload returns the workload variant the plan persisted in the
// placement document. Legacy rows without an index fall back to fleet-based
// selection.
func (s *Service) selectWorkload(ctx context.Context, row db.GetDeploymentRow, m *recipe.Manifest) (int, *recipe.Workload, error) {
	if wi := ParsePlacementSet(row.Placement).Workload; wi != nil {
		if *wi < 0 || *wi >= len(m.Workloads) {
			return 0, nil, fmt.Errorf("workload index %d out of range for recipe", *wi)
		}
		return *wi, &m.Workloads[*wi], nil
	}
	return m.SelectWorkload(s.targetFor(ctx, row))
}

// ---------------------------------------------------------------- spec rendering

// renderSpec builds the agent container spec for one rank. Artifact mounts
// always resolve to the node's actual valid placement path; a missing
// placement is an error, never a guess.
func (s *Service) renderSpec(ctx context.Context, depID string, rank int32, runID string, pl *Placement) (*runtime.ContainerSpec, error) {
	if pl == nil {
		return nil, fmt.Errorf("no placement for rank %d", rank)
	}
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, err
	}
	m, err := s.manifestFor(ctx, row.RecipeDigest)
	if err != nil {
		return nil, err
	}
	_, w, err := s.selectWorkload(ctx, row, m)
	if err != nil {
		return nil, err
	}
	values, err := m.ProfileValues(row.Profile)
	if err != nil {
		return nil, err
	}
	rctx := recipe.RenderContext{
		NodeID:    depID,
		NodeRank:  int(rank),
		Artifacts: map[string]string{},
		Profiles:  values,
	}
	for _, a := range m.Artifacts {
		dest := a.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + a.Name
		}
		rctx.Artifacts[a.Name] = dest
	}

	spec := &runtime.ContainerSpec{
		Name:            containerName(depID, runID, rank),
		Image:           w.Image.Reference,
		ImageDigest:     w.Image.Digest,
		Entrypoint:      w.Command,
		NetworkMode:     "bridge",
		NoNewPrivileges: true,
		CapDrop:         []string{"ALL"},
	}
	for _, a := range w.Args {
		if rendered, rerr := m.Render(a, rctx); rerr == nil {
			spec.Cmd = append(spec.Cmd, rendered)
		} else {
			spec.Cmd = append(spec.Cmd, a)
		}
	}
	for k, ev := range w.Env {
		if rendered, rerr := m.Render(ev, rctx); rerr == nil {
			spec.Env = append(spec.Env, k+"="+rendered)
		} else {
			spec.Env = append(spec.Env, k+"="+ev)
		}
	}
	sort.Strings(spec.Env)
	if w.NetworkMode != "" {
		spec.NetworkMode = w.NetworkMode
	}
	if w.Resources.CPU > 0 {
		spec.CPU = w.Resources.CPU
	}
	if w.Resources.MemoryBytes > 0 {
		spec.MemoryBytes = w.Resources.MemoryBytes
	}
	if w.Resources.ShmBytes > 0 {
		spec.ShmBytes = w.Resources.ShmBytes
	}
	if w.Resources.Pids > 0 {
		spec.PidsLimit = w.Resources.Pids
	}
	if len(pl.Accelerators) > 1 {
		spec.GPUDeviceIDs = append(spec.GPUDeviceIDs, pl.Accelerators...)
	} else if pl.AcceleratorUUID != "" {
		spec.GPUDeviceIDs = append(spec.GPUDeviceIDs, pl.AcceleratorUUID)
	}
	if w.Devices != nil {
		if w.Devices.Accelerator != nil && w.Devices.Accelerator.All {
			spec.GPUsAll = true
		}
		if w.Devices.RDMA != nil && len(w.Devices.RDMA.Devices) > 0 {
			spec.RDMAPaths = w.Devices.RDMA.Devices
		} else if w.Devices.RDMA != nil && w.Devices.RDMA.All {
			spec.RDMAPaths = []string{"/dev/infiniband"}
		}
	}
	for _, p := range w.Ports {
		base := p.Host
		if base == 0 {
			base = p.Container
		}
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		spec.Ports = append(spec.Ports, runtime.PortSpec{
			Host:      base + int(rank),
			Container: p.Container,
			Protocol:  proto,
		})
	}
	for _, a := range m.Artifacts {
		dest := a.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + a.Name
		}
		art, aerr := s.q.GetArtifactByIdentity(ctx, a.Source.Identity)
		if aerr != nil {
			return nil, fmt.Errorf("artifact %s: unknown identity %s", a.Name, a.Source.Identity)
		}
		source, ok := s.validPlacement(ctx, art.ID, pl.NodeID)
		if !ok {
			return nil, fmt.Errorf("artifact %s: no valid placement on node %s", a.Name, pl.NodeID)
		}
		spec.Mounts = append(spec.Mounts, runtime.MountSpec{
			Source:   source,
			Dest:     dest,
			ReadOnly: true,
		})
	}
	// Privileged recipes drop the hardening defaults.
	perm := map[string]bool{}
	for _, p := range w.Permissions {
		perm[p] = true
	}
	if perm["privileged"] || perm["no_new_privileges"] {
		spec.NoNewPrivileges = false
		spec.CapDrop = nil
	}
	spec.Labels = map[string]string{
		runtime.LabelManaged:       "true",
		runtime.LabelDeployment:    depID,
		runtime.LabelRun:           runID,
		runtime.LabelRecipe:        row.RecipeDigest,
		runtime.LabelRecipeVersion: recipeVersion(row.RecipeDigest),
		runtime.LabelRank:          fmt.Sprintf("%d", rank),
		runtime.LabelModule:        "serving",
	}
	return spec, nil
}

func containerName(depID, runID string, rank int32) string {
	return fmt.Sprintf("lmw-%s-%s-r%d", shortID(depID), shortID(runID), rank)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "deployment:")
	id = strings.TrimPrefix(id, "run:")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func recipeVersion(digest string) string {
	if len(digest) >= 12 {
		return digest[:12]
	}
	return digest
}

func (s *Service) manifestFor(ctx context.Context, digest string) (*recipe.Manifest, error) {
	row, err := s.q.GetRecipe(ctx, digest)
	if err != nil {
		return nil, err
	}
	return recipe.Parse([]byte(row.Manifest))
}

func (s *Service) targetFor(ctx context.Context, row db.GetDeploymentRow) recipe.Target {
	t := recipe.Target{NodeCount: 1}
	ps := ParsePlacementSet(row.Placement)
	var first *Placement
	for _, pl := range ps.Entries {
		first = &pl
		break
	}
	if first == nil {
		return t
	}
	if n, err := s.q.GetNode(ctx, first.NodeID); err == nil && n.Inventory.Valid {
		var inv inventory.Inventory
		if json.Unmarshal([]byte(n.Inventory.String), &inv) == nil && len(inv.Accelerators) > 0 {
			t.Vendor = inv.Accelerators[0].Vendor
			t.Architecture = inv.Accelerators[0].Architecture
			t.Features = inv.Accelerators[0].Features
		}
	}
	return t
}

// AllRanks returns every rank in the placement (Entries authoritative,
// legacy Ranks map fallback), sorted.
func (ps placementSet) AllRanks() []int32 {
	var out []int32
	if len(ps.Entries) > 0 {
		for _, e := range ps.Entries {
			out = append(out, e.Rank)
		}
	} else {
		for _, r := range ps.Ranks {
			out = append(out, int32(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mustJSON renders an event payload; encoding errors fall back to "{}".
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
