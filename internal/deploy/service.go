package deploy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
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
	// transferInflight keys dep|node|artifact to active preparation commands.
	transferInflight   map[string]string
	rankStates         map[string]map[int32]string
	dispatchMu         sync.Mutex
	updateMu           sync.Mutex
	updateLive         map[string]bool
	updateFetchWaiters map[string]chan error
}

func New(dbh *sql.DB, q *db.Queries, bus *events.EventBus, runsSvc *runs.Service, nodes NodeSender, ca *ca.CA) *Service {
	return &Service{
		db:                 dbh,
		q:                  q,
		bus:                bus,
		runs:               runsSvc,
		nodes:              nodes,
		ca:                 ca,
		inflight:           map[string]*inflightCmd{},
		transferInflight:   map[string]string{},
		rankStates:         map[string]map[int32]string{},
		updateLive:         map[string]bool{},
		updateFetchWaiters: map[string]chan error{},
	}
}

// ---------------------------------------------------------------- planning

// Plan previews a deployment from the current fleet state.
func (s *Service) Plan(ctx context.Context, req PlanRequest) (*Plan, error) {
	return s.plan(ctx, req, nil, false)
}

func (s *Service) plan(ctx context.Context, req PlanRequest, ignoredDeployments map[string]bool, allowUntrusted bool) (*Plan, error) {
	return s.planWithRecipe(ctx, req, ignoredDeployments, allowUntrusted, nil)
}

func (s *Service) planWithRecipe(ctx context.Context, req PlanRequest, ignoredDeployments map[string]bool, allowUntrusted bool, recipeCandidate *recipe.RepositoryCandidate) (*Plan, error) {
	var (
		m             *recipe.Manifest
		recipeName    string
		recipeVersion string
	)
	if recipeCandidate == nil {
		row, err := s.q.GetRecipe(ctx, req.RecipeDigest)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrRecipe, req.RecipeDigest)
		}
		// Trust gate: an untrusted recipe is inspectable but not launchable.
		// Plan (and thus Create) blocks before any run or deployment row exists.
		if row.TrustState == recipe.TrustUntrusted && !allowUntrusted {
			return nil, fmt.Errorf("%w: %s: approve the permission diff or verify a signature first", ErrUntrusted, req.RecipeDigest)
		}
		m, err = recipe.Parse([]byte(row.Manifest))
		if err != nil {
			return nil, fmt.Errorf("recipe manifest: %w", err)
		}
		recipeName = row.Name
		recipeVersion = row.Version
	} else {
		if recipeCandidate.Manifest == nil || recipeCandidate.Digest != req.RecipeDigest {
			return nil, fmt.Errorf("%w: invalid repository candidate", ErrRecipe)
		}
		if !allowUntrusted {
			return nil, fmt.Errorf("%w: %s: approve the permission diff or verify a signature first", ErrUntrusted, req.RecipeDigest)
		}
		m = recipeCandidate.Manifest
		recipeName = m.Metadata.Name
		recipeVersion = m.Metadata.Version
	}
	values, err := m.ProfileValues(req.Profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProfile, err)
	}

	plan := &Plan{
		RecipeDigest:  req.RecipeDigest,
		RecipeName:    recipeName,
		RecipeVersion: recipeVersion,
		Profile:       req.Profile,
		Variants:      req.Variants,
	}

	nodes, err := s.q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	leased := map[string]db.ActiveLeasesWithOwnersRow{}
	leases, err := s.q.ActiveLeasesWithOwners(ctx)
	if err != nil {
		return nil, err
	}
	for _, lease := range leases {
		if lease.OwnerKind == "deployment" && ignoredDeployments[lease.OwnerID] {
			continue
		}
		leased[lease.Resource] = lease
	}

	reqAcc := s.accelRequirement(m)

	// Try workload variants in order; the first one the fleet can satisfy wins.
	var (
		wi  int
		w   *recipe.Workload
		try bool
	)
	if len(nodes) == 0 && len(m.Workloads) > 0 {
		wi, w, try = 0, &m.Workloads[0], true
	}
	for i := range m.Workloads {
		if try {
			break
		}
		variant := &m.Workloads[i]
		nodeCount := s.workloadNodeCount(m, variant)
		ok, err := variantMatchesNodes(variant, nodeCount, nodes, req.Placements)
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
	blockedAccelerators := map[string]Conflict{}
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
				if w.Match != nil && w.Match.Accelerator != nil &&
					!matchesWorkloadAccelerator(w, s.workloadNodeCount(m, w), a) {
					continue
				}
				resource := "gpu:" + n.ID + ":" + a.UUID
				if owner, occupied := leased[resource]; occupied {
					relevant := len(devIndices) == 0
					for _, index := range devIndices {
						if index == int(a.Index) {
							relevant = true
							break
						}
					}
					if relevant {
						conflict := Conflict{Resource: resource, OccupiedBy: owner.OwnerID}
						if owner.OwnerKind == "deployment" {
							conflict.DeploymentID = owner.OwnerID
						}
						blockedAccelerators[resource] = conflict
					}
					continue
				}
				c.free = append(c.free, a)
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
	if len(placements) < len(rankList) {
		resources := make([]string, 0, len(blockedAccelerators))
		for resource := range blockedAccelerators {
			resources = append(resources, resource)
		}
		sort.Strings(resources)
		for _, resource := range resources {
			plan.Conflicts = append(plan.Conflicts, blockedAccelerators[resource])
		}
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
					NodeID:        pl.NodeID,
					NodeName:      pl.NodeName,
					HostPort:      int32(hostPort),
					ContainerPort: int32(p.Container),
					Protocol:      proto,
				})
				k := portKey{addr: addr, port: hostPort}
				planPorts[k] = append(planPorts[k], pl.NodeID)
				resource := fmt.Sprintf("port:%s:%d", pl.NodeID, hostPort)
				if owner, occupied := leased[resource]; occupied {
					conflict := Conflict{Resource: resource, OccupiedBy: owner.OwnerID}
					if owner.OwnerKind == "deployment" {
						conflict.DeploymentID = owner.OwnerID
					}
					plan.Conflicts = append(plan.Conflicts, conflict)
				}
			}
		}
		// Active-deployment endpoint conflicts.
		active, err := s.q.ListActiveDeployments(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range active {
			if ignoredDeployments[d.ID] {
				continue
			}
			if !d.Endpoint.Valid || d.Endpoint.String == "" {
				continue
			}
			otherAddr, otherPortText, splitErr := net.SplitHostPort(d.Endpoint.String)
			if splitErr != nil {
				continue
			}
			otherPort, parseErr := strconv.Atoi(otherPortText)
			if parseErr != nil {
				continue
			}
			if nodeIDs, hit := planPorts[portKey{addr: otherAddr, port: otherPort}]; hit {
				for _, nID := range nodeIDs {
					plan.Conflicts = append(plan.Conflicts, Conflict{
						Resource:     fmt.Sprintf("port:%s:%d", nID, otherPort),
						DeploymentID: d.ID,
						OccupiedBy:   d.ID,
					})
				}
			}
		}
		// A deployment row carries the actionable owner for its lease. Remove
		// the generic lease duplicate for the same resource.
		owned := map[string]bool{}
		for _, conflict := range plan.Conflicts {
			if conflict.DeploymentID != "" {
				owned[conflict.Resource] = true
			}
		}
		filtered := plan.Conflicts[:0]
		for _, conflict := range plan.Conflicts {
			if conflict.DeploymentID == "" && owned[conflict.Resource] {
				continue
			}
			filtered = append(filtered, conflict)
		}
		plan.Conflicts = filtered
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
			model := profileString(values, "model")
			if model == "" {
				model = m.Metadata.Model
			}
			ep := Endpoint{
				Host:  addr,
				Port:  int32(base + int(r0.Rank)),
				Model: model,
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
		src, srcErr := a.EffectiveSource(req.Variants[a.Name])
		if srcErr != nil {
			return nil, fmt.Errorf("artifact %s: %w", a.Name, srcErr)
		}
		identity, identityErr := artifactidentity.Canonical(
			src.Type, src.Identity, src.Revision, src.Digest,
		)
		if identityErr != nil {
			return nil, fmt.Errorf("artifact %s: %w", a.Name, identityErr)
		}
		dest := a.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + a.Name
		}
		art, aerr := s.q.GetArtifactByIdentity(ctx, identity)
		if aerr != nil {
			// Unknown artifact: nothing in the fleet holds it; the library
			// must install it first. Blocking.
			plan.Transfers = append(plan.Transfers, TransferPreview{
				ArtifactID: identity,
				Identity:   identity,
				SourceNode: "origin",
				DestNode:   "all",
				DestPath:   dest,
			})
			plan.Risks = append(plan.Risks, "artifact:"+a.Name+":origin_download")
			plan.Diagnostics = append(plan.Diagnostics, diag.Error("artifact.unplaced",
				"no node holds "+identity+"; install it via the library first"))
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
		if srcName == "" {
			parsed, parseErr := artifactidentity.Parse(art.Identity)
			if parseErr == nil && parsed.Kind == "model" {
				srcName = "origin"
				plan.Risks = append(plan.Risks, "artifact:"+a.Name+":origin_download")
			} else {
				plan.Diagnostics = append(plan.Diagnostics, diag.Error("artifact.unplaced",
					"no node holds a valid copy of "+art.Identity))
			}
		}
		for _, name := range missing {
			plan.Transfers = append(plan.Transfers, TransferPreview{
				ArtifactID: art.ID, Identity: art.Identity, SourceNode: srcName,
				DestNode: name, DestPath: dest, Bytes: artifactSize(art.Metadata),
			})
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

func variantMatchesNodes(workload *recipe.Workload, nodeCount int, nodes []db.Node, overrides []PlacementOverride) (bool, error) {
	required := map[string]bool{}
	for _, override := range overrides {
		required[override.NodeID] = true
	}
	matches := 0
	for _, node := range nodes {
		if node.Status != "online" || (len(required) > 0 && !required[node.ID]) {
			continue
		}
		if workload.Match == nil {
			matches++
			continue
		}
		if workload.Match.Accelerator == nil {
			ok, _ := workload.Match.SatisfiedBy(recipe.Target{NodeCount: nodeCount})
			if ok {
				matches++
			}
			continue
		}
		if !node.Inventory.Valid {
			continue
		}
		var inv inventory.Inventory
		if err := json.Unmarshal([]byte(node.Inventory.String), &inv); err != nil {
			continue
		}
		if variantMatchesInventory(workload, nodeCount, &inv) {
			matches++
		}
	}
	if len(required) > 0 {
		return matches == len(required), nil
	}
	return matches >= nodeCount, nil
}

func variantMatchesInventory(workload *recipe.Workload, nodeCount int, inv *inventory.Inventory) bool {
	if workload.Match == nil {
		return true
	}
	if workload.Match.Accelerator == nil {
		ok, _ := workload.Match.SatisfiedBy(recipe.Target{NodeCount: nodeCount})
		return ok
	}
	accelerators := inv.Accelerators
	requireAll := false
	if workload.Devices != nil && workload.Devices.Accelerator != nil {
		device := workload.Devices.Accelerator
		requireAll = device.All
		if len(device.Indices) > 0 {
			accelerators = nil
			for _, index := range device.Indices {
				for _, accelerator := range inv.Accelerators {
					if int(accelerator.Index) == index {
						accelerators = append(accelerators, accelerator)
					}
				}
			}
			if len(accelerators) != len(device.Indices) {
				return false
			}
			requireAll = true
		}
	}
	if len(accelerators) == 0 {
		return false
	}
	matched := 0
	for _, accelerator := range accelerators {
		if matchesWorkloadAccelerator(workload, nodeCount, accelerator) {
			matched++
		} else if requireAll {
			return false
		}
	}
	return matched > 0
}

func matchesWorkloadAccelerator(workload *recipe.Workload, nodeCount int, accelerator inventory.Accelerator) bool {
	target := recipe.Target{
		NodeCount: nodeCount, Vendor: accelerator.Vendor,
		Architecture: accelerator.Architecture, Features: accelerator.Features,
	}
	ok, err := workload.Match.SatisfiedBy(target)
	return err == nil && ok
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
	// Variants maps artifact name -> selected model variant, captured at plan
	// time so render re-resolves the exact artifact identity the plan used.
	Variants map[string]string `json:"variants,omitempty"`
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

func placementSetFromPlan(plan *Plan) placementSet {
	workload := plan.WorkloadIndex
	placements := placementSet{
		Ranks:    make(map[string]int, len(plan.Placements)),
		Entries:  plan.Placements,
		Workload: &workload,
		Variants: plan.Variants,
	}
	for _, placement := range plan.Placements {
		placements.Ranks[placement.NodeID] = int(placement.Rank)
	}
	return placements
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
		Variants:     req.Variants,
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

	return s.createPlanned(ctx, plan)
}

func (s *Service) createPlanned(ctx context.Context, plan *Plan) (*Deployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	qtx := db.New(tx)

	depID, _ := id.New()
	runIDStr, _ := id.New()
	ps := placementSetFromPlan(plan)
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
		endpoint = net.JoinHostPort(plan.Endpoint.Host, strconv.Itoa(int(plan.Endpoint.Port)))
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
	if endpoint != "" {
		if err := qtx.UpdateDeploymentEndpointMetadata(ctx, db.UpdateDeploymentEndpointMetadataParams{
			EndpointModel: sql.NullString{String: plan.Endpoint.Model, Valid: plan.Endpoint.Model != ""},
			EndpointPath:  sql.NullString{String: plan.Endpoint.Path, Valid: plan.Endpoint.Path != ""},
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
	_ = s.runs.SetState(ctx, runIDStr, runs.Waiting, "", "")
	for _, pl := range plan.Placements {
		s.dispatchNext(ctx, depID, pl.Rank, runIDStr, pl)
	}

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

func (s *Service) inflightHas(depID string, rank int32, op string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.inflight {
		if command.DepID == depID && command.Rank == rank && command.Op == op {
			return true
		}
	}
	return false
}

func (s *Service) inflightDrop(depID string, rank int32, operations ...string) {
	drop := map[string]bool{}
	for _, operation := range operations {
		drop[operation] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for commandID, command := range s.inflight {
		if command.DepID == depID && command.Rank == rank && drop[command.Op] {
			delete(s.inflight, commandID)
		}
	}
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

func (s *Service) sendExtension(ctx context.Context, row db.GetDeploymentRow, placement Placement, rank int32, runID, phase string, extension *recipe.Extension) error {
	if s.inflightHas(row.ID, rank, phase) {
		return nil
	}
	spec, err := s.renderExtensionSpec(ctx, row, placement, rank, runID, extension)
	if err != nil {
		return err
	}
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	commandID, _ := id.New()
	s.inflightMark(commandID, row.ID, rank, phase)
	if phase == "prepare" {
		s.setPhase(ctx, row.ID, rank, PhasePreparing)
	} else {
		s.setPhase(ctx, row.ID, rank, PhaseVerifying)
	}
	timeout := extension.TimeoutSeconds
	if timeout == 0 {
		timeout = 900
	}
	outputLimit := extension.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = 1 << 20
	}
	outputSchema, err := json.Marshal(extension.OutputSchema)
	if err != nil {
		return err
	}
	if phase == "verify" && runID != "" {
		_ = s.runs.SetState(ctx, runID, runs.Verifying, "", "")
	}
	if !s.nodes.Send(placement.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_ExtensionCommand{
		ExtensionCommand: &agentv1.ExtensionCommand{
			CommandId: commandID, Phase: phase, DeploymentId: row.ID, RunId: runID, Rank: rank,
			ContainerSpec: specBytes, TimeoutSeconds: uint32(timeout), OutputLimitBytes: uint32(outputLimit),
			OutputSchema: outputSchema,
		},
	}}) {
		return fmt.Errorf("node %s is offline", placement.NodeID)
	}
	return nil
}

func (s *Service) stopExtension(ctx context.Context, row db.GetDeploymentRow, placement Placement, rank int32, runID, op string) {
	if s.inflightHas(row.ID, rank, op) {
		return
	}
	s.inflightDrop(row.ID, rank, "prepare", "verify")
	commandID, _ := id.New()
	s.inflightMark(commandID, row.ID, rank, op)
	if !s.nodes.Send(placement.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_ExtensionCommand{
		ExtensionCommand: &agentv1.ExtensionCommand{
			CommandId: commandID, Phase: "stop", DeploymentId: row.ID, RunId: runID, Rank: rank,
		},
	}}) {
		s.noteDispatch(ctx, row.ID, diag.Error("workload.node_offline", fmt.Sprintf("node %s offline; extension stop paused", placement.NodeID)))
	}
}

func (s *Service) renderExtensionSpec(ctx context.Context, row db.GetDeploymentRow, placement Placement, rank int32, runID string, extension *recipe.Extension) (*runtime.ContainerSpec, error) {
	manifest, err := s.manifestFor(ctx, row.RecipeDigest)
	if err != nil {
		return nil, err
	}
	base, err := s.renderSpec(ctx, row.ID, rank, runID, &placement)
	if err != nil {
		return nil, err
	}
	values, err := manifest.ProfileValues(row.Profile)
	if err != nil {
		return nil, err
	}
	renderContext := recipe.RenderContext{
		NodeID: placement.NodeID, NodeRank: int(rank), Artifacts: map[string]string{}, Profiles: values,
	}
	for _, artifact := range manifest.Artifacts {
		dest := artifact.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + artifact.Name
		}
		identity, err := canonicalArtifactIdentity(artifact, variantsFor(row)[artifact.Name])
		if err != nil {
			return nil, err
		}
		if parsed, err := artifactidentity.Parse(identity); err == nil && parsed.Revision != "" {
			dest = filepath.ToSlash(filepath.Join(dest, "snapshots", parsed.Revision))
		}
		renderContext.Artifacts[artifact.Name] = dest
	}
	spec := &runtime.ContainerSpec{
		Image: extension.Image.Reference, ImageDigest: extension.Image.Digest,
		Entrypoint: extension.Command, NetworkMode: "none", ReadonlyRootfs: true,
		NoNewPrivileges: true, CapDrop: []string{"ALL"},
		CPU: 1, MemoryBytes: 1 << 30, PidsLimit: 256, TmpfsBytes: 64 << 20,
		Mounts: append([]runtime.MountSpec(nil), base.Mounts...),
		Labels: runtime.ManagedLabels(row.ID, runID, row.RecipeDigest, recipeVersion(row.RecipeDigest), int(rank), "extension"),
	}
	if extension.Network == "egress" {
		spec.NetworkMode = "bridge"
	}
	for _, argument := range extension.Args {
		rendered, err := manifest.Render(argument, renderContext)
		if err != nil {
			return nil, err
		}
		spec.Cmd = append(spec.Cmd, rendered)
	}
	for key, value := range extension.Env {
		rendered, err := manifest.Render(value, renderContext)
		if err != nil {
			return nil, err
		}
		spec.Env = append(spec.Env, key+"="+rendered)
	}
	sort.Strings(spec.Env)
	return spec, nil
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
//	  started  -> phase stopping, then STOP
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
			if !s.ensureRecipePackage(ctx, row, rank, pl) || !s.ensureArtifacts(ctx, row, rank, pl) {
				return
			}
			manifest, manifestErr := s.manifestFor(ctx, row.RecipeDigest)
			if manifestErr != nil {
				s.failDispatch(ctx, depID, rank, runID, "recipe.manifest", manifestErr.Error())
				return
			}
			if manifest.Prepare != nil {
				if err := s.sendExtension(ctx, row, pl, rank, runID, "prepare", manifest.Prepare); err != nil {
					s.failDispatch(ctx, depID, rank, runID, "extension.prepare_failed", err.Error())
				}
				return
			}
			s.setPhase(ctx, depID, rank, PhasePrepared)
			s.dispatchNext(ctx, depID, rank, runID, pl)
			return
		case PhasePreparing:
			manifest, manifestErr := s.manifestFor(ctx, row.RecipeDigest)
			if manifestErr != nil || manifest.Prepare == nil {
				s.failDispatch(ctx, depID, rank, runID, "extension.prepare_failed", "prepare extension is unavailable")
				return
			}
			if err := s.sendExtension(ctx, row, pl, rank, runID, "prepare", manifest.Prepare); err != nil {
				s.noteDispatch(ctx, depID, diag.Error("extension.prepare_failed", err.Error()))
			}
		case PhasePrepared:
			if !s.ensureRecipePackage(ctx, row, rank, pl) || !s.ensureArtifacts(ctx, row, rank, pl) {
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
		case PhaseVerifying:
			manifest, manifestErr := s.manifestFor(ctx, row.RecipeDigest)
			if manifestErr != nil || manifest.Verify == nil {
				s.failDispatch(ctx, depID, rank, runID, "extension.verify_failed", "verify extension is unavailable")
				return
			}
			if err := s.sendExtension(ctx, row, pl, rank, runID, "verify", manifest.Verify); err != nil {
				s.noteDispatch(ctx, depID, diag.Error("extension.verify_failed", err.Error()))
			}
		}
	case "stopped":
		switch phase {
		case PhasePreparing:
			s.stopExtension(ctx, row, pl, rank, runID, "stop-prepare")
		case PhasePrepared:
			s.setPhase(ctx, depID, rank, PhaseStopped)
			s.checkStopComplete(ctx, depID, runID)
		case PhaseVerifying:
			s.stopExtension(ctx, row, pl, rank, runID, "stop-verify")
		case PhaseNone:
			// PULL never acked: CREATE/START never sent, no container can
			// exist. Confirm stopped directly.
			s.setPhase(ctx, depID, rank, PhaseStopped)
			s.checkStopComplete(ctx, depID, runID)
		case PhasePulled, PhaseCreated, PhaseStarted, PhaseStopping:
			if phase == PhaseStarted {
				s.setPhase(ctx, depID, rank, PhaseStopping)
			}
			cmdID, _ := id.New()
			s.inflightMark(cmdID, depID, rank, "stop")
			_ = s.sendWorkload(nodeID, cmdID, agentv1.WorkloadOp_WORKLOAD_OP_STOP, depID, runID, rank, nil)
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
	if c.Op == "repository-update-fetch" {
		s.completeRepositoryUpdateFetch(cr.CommandId, cr.Ok, cr.Error)
		return
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
	case "prepare":
		if !cr.Ok {
			s.failDispatch(ctx, depID, rank, runID, "extension.prepare_failed", cr.Error)
			return
		}
		s.setPhase(ctx, depID, rank, PhasePrepared)
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "stop-prepare":
		if !cr.Ok {
			s.failDispatch(ctx, depID, rank, runID, "extension.stop_failed", cr.Error)
			return
		}
		s.setPhase(ctx, depID, rank, PhaseStopped)
		s.checkStopComplete(ctx, depID, runID)
	case "stop-verify":
		if !cr.Ok {
			s.failDispatch(ctx, depID, rank, runID, "extension.stop_failed", cr.Error)
			return
		}
		s.setPhase(ctx, depID, rank, PhaseStarted)
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "verify":
		if !cr.Ok {
			s.failDispatch(ctx, depID, rank, runID, "extension.verify_failed", cr.Error)
			return
		}
		s.setPhase(ctx, depID, rank, PhaseStarted)
		if runID != "" {
			_ = s.runs.SetState(ctx, runID, runs.Running, "", "")
		}
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "artifact-fetch":
		if cr.Ok {
			return // placement report precedes the success result on the agent stream
		}
		s.clearTransferCommand(cr.CommandId)
		s.failDispatch(ctx, depID, rank, runID, "artifact.fetch_failed", cr.Error)
		return
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
		manifest, manifestErr := s.manifestFor(ctx, row.RecipeDigest)
		if manifestErr != nil {
			s.failDispatch(ctx, depID, rank, runID, "recipe.manifest", manifestErr.Error())
			return
		}
		if manifest.Verify != nil {
			if err := s.sendExtension(ctx, row, pl, rank, runID, "verify", manifest.Verify); err != nil {
				s.failDispatch(ctx, depID, rank, runID, "extension.verify_failed", err.Error())
			}
			return
		}
		s.setPhase(ctx, depID, rank, PhaseStarted)
		if runID != "" {
			_ = s.runs.SetState(ctx, runID, runs.Running, "", "")
		}
		s.dispatchNext(ctx, depID, rank, runID, pl)
	case "inspect":
		if cr.Ok {
			s.OnStateUpdate(ctx, pl.NodeID, &agentv1.StateUpdate{
				DeploymentId:      depID,
				ContainerId:       cr.ContainerId,
				State:             cr.ContainerState,
				Rank:              rank,
				DiagnosticMessage: cr.Error,
				ExitCode:          cr.ExitCode,
				OomKilled:         cr.OomKilled,
			})
		} else if isContainerMissing(cr.Error) {
			s.OnStateUpdate(ctx, pl.NodeID, &agentv1.StateUpdate{
				DeploymentId:      depID,
				State:             "missing",
				Rank:              rank,
				DiagnosticCode:    "container.missing",
				DiagnosticMessage: cr.Error,
			})
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

func (s *Service) clearTransferCommand(commandID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, activeID := range s.transferInflight {
		if activeID == commandID {
			delete(s.transferInflight, key)
		}
	}
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

// variantsFor parses the deployment's placement JSON and returns the
// artifact->variant map captured at plan time (empty map when none).
func variantsFor(row db.GetDeploymentRow) map[string]string {
	out := map[string]string{}
	if row.Placement == "" {
		return out
	}
	var ps placementSet
	if json.Unmarshal([]byte(row.Placement), &ps) != nil {
		return out
	}
	if ps.Variants != nil {
		return ps.Variants
	}
	return out
}
func canonicalArtifactIdentity(artifact recipe.Artifact, variant string) (string, error) {
	src, err := artifact.EffectiveSource(variant)
	if err != nil {
		return "", err
	}
	return artifactidentity.Canonical(
		src.Type, src.Identity, src.Revision, src.Digest,
	)
}

// ensureArtifacts gates container dispatch on every recipe artifact having
// a valid placement on this rank's node. Missing copies are filled with a
// peer transfer from a node that already holds one. Returns true when all
// artifacts are placed; false when a transfer is in flight (placement and
// ack events re-drive dispatch) or the deployment was failed.
func (s *Service) ensureRecipePackage(ctx context.Context, row db.GetDeploymentRow, rank int32, placement Placement) bool {
	identity := "recipe://" + row.RecipeDigest
	artifact, err := s.q.GetArtifactByIdentity(ctx, identity)
	if err != nil {
		s.failDispatch(ctx, row.ID, rank, s.runIDFor(ctx, row.ID), "recipe.package_missing", err.Error())
		return false
	}
	if _, valid := s.validPlacement(ctx, artifact.ID, placement.NodeID); valid {
		return true
	}
	key := row.ID + "|" + placement.NodeID + "|" + identity
	s.mu.Lock()
	if s.transferInflight[key] != "" {
		s.mu.Unlock()
		return false
	}
	s.transferInflight[key] = "pending"
	s.mu.Unlock()
	commandID, _ := id.New()
	s.inflightMark(commandID, row.ID, rank, "artifact-fetch")
	if !s.nodes.Send(placement.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_ArtifactCommand{
		ArtifactCommand: &agentv1.ArtifactCommand{
			CommandId: commandID, Op: agentv1.ArtifactOp_ARTIFACT_OP_FETCH, ArtifactIdentity: identity,
		},
	}}) {
		s.inflightTake(commandID)
		s.mu.Lock()
		delete(s.transferInflight, key)
		s.mu.Unlock()
		s.failDispatch(ctx, row.ID, rank, s.runIDFor(ctx, row.ID), "recipe.package_node_offline", "node offline")
		return false
	}
	s.mu.Lock()
	s.transferInflight[key] = commandID
	s.mu.Unlock()
	s.bus.Publish(ctx, "recipe.package_delivery", placement.NodeID, mustJSON(map[string]any{
		"command_id": commandID, "recipe": row.RecipeDigest,
	}))
	return false
}

func (s *Service) ensureArtifacts(ctx context.Context, row db.GetDeploymentRow, rank int32, pl Placement) bool {
	m, err := s.manifestFor(ctx, row.RecipeDigest)
	if err != nil {
		s.noteDispatch(ctx, row.ID, diag.Error("recipe.manifest", err.Error()))
		return false
	}
	runID := s.runIDFor(ctx, row.ID)
	for _, a := range m.Artifacts {
		identity, err := canonicalArtifactIdentity(a, variantsFor(row)[a.Name])
		if err != nil {
			s.failDispatch(ctx, row.ID, rank, runID, "artifact.identity", err.Error())
			return false
		}
		art, err := s.q.GetArtifactByIdentity(ctx, identity)
		if err != nil {
			s.failDispatch(ctx, row.ID, rank, runID, "artifact.unplaced",
				"no node holds "+identity)
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
		s.transferInflight[key] = "pending"
		s.mu.Unlock()
		var tid string
		var dispatchErr error
		if s.transferSourceName(ctx, art.ID) == "" {
			tid, dispatchErr = s.startOriginFetch(ctx, art, pl.NodeID, row.ID, rank)
		} else {
			tid, dispatchErr = s.startTransfer(ctx, art, "", pl.NodeID, "", row.Fabric.String, runID)
		}
		if dispatchErr != nil {
			s.mu.Lock()
			delete(s.transferInflight, key)
			s.mu.Unlock()
			s.failDispatch(ctx, row.ID, rank, runID, "artifact.transfer_failed", dispatchErr.Error())
			return false
		}
		s.mu.Lock()
		s.transferInflight[key] = tid
		s.mu.Unlock()
		s.bus.Publish(ctx, "transfer.started", pl.NodeID, mustJSON(map[string]any{
			"transfer_id": tid, "artifact": art.Identity, "dest_node": pl.NodeID,
		}))
		return false
	}
	return true
}

func (s *Service) startOriginFetch(ctx context.Context, artifact db.Artifact, nodeID, depID string, rank int32) (string, error) {
	parsed, err := artifactidentity.Parse(artifact.Identity)
	if err != nil || parsed.Kind != "model" {
		return "", fmt.Errorf("artifact origin is unsupported for %s", artifact.Identity)
	}
	commandID, _ := id.New()
	s.inflightMark(commandID, depID, rank, "artifact-fetch")
	message := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_ArtifactCommand{
		ArtifactCommand: &agentv1.ArtifactCommand{
			CommandId: commandID, Op: agentv1.ArtifactOp_ARTIFACT_OP_FETCH,
			ArtifactIdentity: artifact.Identity,
		},
	}}
	if !s.nodes.Send(nodeID, message) {
		s.inflightTake(commandID)
		return "", fmt.Errorf("destination node %s is offline", nodeID)
	}
	s.bus.Publish(ctx, "artifact.fetch_started", nodeID, mustJSON(map[string]any{
		"command_id": commandID, "artifact": artifact.Identity,
	}))
	return commandID, nil
}

// StartTransfer plans and dispatches a peer transfer of art from an
// explicit source node to destNodeID at destPath. destPath must be a safe
// relative path (nested paths preserved; absolute or ".." rejected); an
// empty destPath derives it from the source placement.
func (s *Service) StartTransfer(ctx context.Context, art db.Artifact, sourceNodeID, destNodeID, destPath string) (string, error) {
	return s.startTransfer(ctx, art, sourceNodeID, destNodeID, destPath, "", "")
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
func (s *Service) startTransfer(ctx context.Context, art db.Artifact, sourceNodeID, destNodeID, destPath, fabric, runID string) (string, error) {
	destRel, err := safeRelPath(destPath)
	if err != nil {
		return "", err
	}
	if _, err := s.q.GetNode(ctx, destNodeID); err != nil {
		return "", err
	}
	if !s.nodes.Online(destNodeID) {
		return "", fmt.Errorf("destination node %s is offline", destNodeID)
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
	peerAddr := ""
	if src.node.Inventory.Valid {
		if inv, parseErr := inventory.Parse(src.node.Inventory.String); parseErr == nil {
			peerAddr = inv.PeerListen
		}
	}
	if peerAddr == "" {
		return "", fmt.Errorf("source node %s advertises no peer address (set LMW_PEER_ADVERTISE)", src.node.ID)
	}
	tid, _ := id.New()
	if destRel == "" {
		destRel = path.Base(src.pl.Path)
	}
	if runID == "" {
		runID = "transfer:" + tid
	}
	cred := &transferCred{
		TransferID: tid, RunID: runID,
		SourceNode: src.node.ID, DestNode: destNodeID,
		ArtifactID: art.Identity, SrcPath: src.pl.Path,
		SrcSize: src.pl.SizeBytes, DestPath: destRel,
		ExpUnix: time.Now().Add(30 * time.Minute).Unix(),
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
	// Destination pulls directly from the source's mTLS Connect service.
	if !s.nodes.Send(destNodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{
		TransferCommand: &agentv1.TransferCommand{
			TransferId: tc.TransferId, Role: "dest", PeerAddress: peerAddr,
			Credential: tc.Credential, ArtifactIdentity: tc.ArtifactIdentity,
			SrcPath: tc.SrcPath, DestPath: tc.DestPath, TimeoutSeconds: tc.TimeoutSeconds,
		},
	}}) {
		_ = s.q.UpdateTransferState(ctx, db.UpdateTransferStateParams{
			State:      "failed",
			Diagnostic: sql.NullString{String: "destination node offline", Valid: true},
			ID:         tid,
		})
		return "", errors.New("destination node offline")
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
	d := diag.Error(code, fmt.Sprintf("rank %d: %s", rank, message)).Res(fmt.Sprintf("rank:%d", rank))
	s.noteDispatch(ctx, depID, d)
	if row, err := s.q.GetDeployment(ctx, depID); err == nil {
		_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
			ObservedState: "failed", Diagnostics: row.Diagnostics, ID: depID,
		})
	}
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
	existing = diag.Upsert(existing, d)
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
			_ = s.q.ClearStoppedDeploymentEndpoint(ctx, db.ClearStoppedDeploymentEndpointParams{
				Diagnostics: diag.Encode(diag.Compact(diag.Decode(d.Diagnostics))),
				ID:          d.ID,
			})
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
		if d.DesiredState == "stopped" {
			s.checkStopComplete(ctx, d.ID, runID)
		}
	}
}

// ---------------------------------------------------------------- state updates
// MarkNodeOffline degrades every desired-running deployment placed on node.
// Leases remain held until container reality is reconciled.
func (s *Service) MarkNodeOffline(ctx context.Context, nodeID string) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	deployments, err := qtx.ListDeployments(ctx)
	if err != nil {
		return
	}
	var changed []string
	for _, deployment := range deployments {
		if deployment.DesiredState != "running" {
			continue
		}
		placements := ParsePlacementSet(deployment.Placement)
		ranks := placements.RanksOnNode(nodeID)
		if len(ranks) == 0 {
			continue
		}
		otherOnline := false
		for _, placement := range placements.Entries {
			if placement.NodeID != nodeID && s.nodes.Online(placement.NodeID) {
				otherOnline = true
			}
		}
		observed := "unknown"
		if otherOnline {
			observed = "degraded"
		}
		diagnostics := diag.Decode(deployment.Diagnostics)
		diagnostics = diag.Upsert(diagnostics, diag.Error(
			"agent.offline", fmt.Sprintf("node %s offline; ranks %v unresolved", nodeID, ranks),
		).Res(nodeID))
		endpoint := deployment.Endpoint
		for _, rank := range ranks {
			s.setRankState(deployment.ID, rank, "offline")
			if rank == 0 {
				endpoint = sql.NullString{String: "", Valid: true}
			}
		}
		if err := qtx.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
			ObservedState: observed, Endpoint: endpoint,
			ModelCapabilities: deployment.ModelCapabilities,
			Diagnostics:       diag.Encode(diagnostics), ID: deployment.ID,
		}); err != nil {
			return
		}
		changed = append(changed, deployment.ID)
	}
	if err := tx.Commit(); err != nil {
		return
	}
	for _, deploymentID := range changed {
		s.bus.Publish(ctx, "deployment.state", deploymentID, mustJSON(map[string]any{
			"deployment_id": deploymentID, "reason": "agent.offline",
		}))
	}
}

func (s *Service) setRankState(deploymentID string, rank int32, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := s.rankStates[deploymentID]
	if states == nil {
		states = map[int32]string{}
		s.rankStates[deploymentID] = states
	}
	states[rank] = state
}

func (s *Service) aggregateRankState(deploymentID string, placements placementSet) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := s.rankStates[deploymentID]
	if len(placements.Entries) == 0 {
		return "unknown"
	}
	running, offline, failed, preparing := 0, 0, 0, 0
	for _, placement := range placements.Entries {
		switch states[placement.Rank] {
		case "running":
			running++
		case "offline", "":
			offline++
		case "created", "restarting":
			preparing++
		case "degraded", "exited", "dead", "missing":
			failed++
		}
	}
	total := len(placements.Entries)
	switch {
	case running == total:
		return "healthy"
	case failed > 0 || (running > 0 && offline > 0):
		return "degraded"
	case running > 0 || preparing > 0:
		return "starting"
	default:
		return "unknown"
	}
}

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

func terminalWorkloadState(state string) bool {
	switch state {
	case "exited", "dead", "missing":
		return true
	default:
		return false
	}
}

func workloadFailure(su *agentv1.StateUpdate) (code, message string) {
	code = "workload." + su.State
	if su.OomKilled {
		code = "workload.oom_killed"
	}
	parts := []string{fmt.Sprintf("rank %d", su.Rank)}
	if su.ContainerId != "" {
		parts = append(parts, fmt.Sprintf("container %s", su.ContainerId))
	}
	parts = append(parts, fmt.Sprintf("state=%s", su.State))
	if su.State == "exited" || su.State == "dead" {
		parts = append(parts, fmt.Sprintf("exit_code=%d", su.ExitCode))
	}
	parts = append(parts, fmt.Sprintf("oom_killed=%t", su.OomKilled))
	if su.DiagnosticMessage != "" {
		parts = append(parts, fmt.Sprintf("error=%s", su.DiagnosticMessage))
	}
	return code, strings.Join(parts, "; ")
}

func (s *Service) failWorkload(ctx context.Context, row db.GetDeploymentRow, su *agentv1.StateUpdate) {
	code, message := workloadFailure(su)
	resource := fmt.Sprintf("rank:%d", su.Rank)
	diagnostics := diag.Upsert(
		diag.Decode(row.Diagnostics),
		diag.Error(code, message).Res(resource),
	)
	if err := s.q.SetDeploymentStopping(ctx, db.SetDeploymentStoppingParams{
		Diagnostics: diag.Encode(diagnostics),
		ID:          row.ID,
	}); err != nil {
		return
	}

	s.setRankState(row.ID, su.Rank, su.State)
	s.setPhase(ctx, row.ID, su.Rank, PhaseStopped)

	runID := ""
	if row.RunID.Valid {
		runID = row.RunID.String
		if run, err := s.runs.Get(ctx, runID); err == nil && !runs.State(run.State).Terminal() {
			_ = s.runs.SetState(ctx, runID, runs.Failed, code, message)
		}
	}

	placements := ParsePlacementSet(row.Placement)
	for _, rank := range placements.AllRanks() {
		if rank == su.Rank {
			continue
		}
		if placement := placements.EntryFor(rank); placement != nil {
			s.dispatchNext(ctx, row.ID, rank, runID, *placement)
		}
	}
	s.checkStopComplete(ctx, row.ID, runID)
	s.bus.Publish(ctx, "deployment.stopping", row.ID, mustJSON(map[string]any{
		"deployment_id": row.ID,
		"reason":        code,
	}))
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
	if row.DesiredState == "running" && terminalWorkloadState(su.State) {
		s.failWorkload(ctx, row, su)
		return
	}
	if row.DesiredState == "stopped" && terminalWorkloadState(su.State) {
		s.setRankState(row.ID, su.Rank, su.State)
		s.setPhase(ctx, row.ID, su.Rank, PhaseStopped)
		runID := ""
		if row.RunID.Valid {
			runID = row.RunID.String
		}
		s.checkStopComplete(ctx, row.ID, runID)
		return
	}
	_, d := mapObserved(su.State, su.DiagnosticMessage)
	rankState := su.State
	if su.DiagnosticMessage != "" {
		rankState = "degraded"
	}
	s.setRankState(row.ID, su.Rank, rankState)
	observed := s.aggregateRankState(row.ID, ParsePlacementSet(row.Placement))
	existing := diag.Decode(row.Diagnostics)
	existing = diag.Upsert(existing, d.Res(fmt.Sprintf("rank:%d", su.Rank)))
	endpoint := row.Endpoint
	if su.EndpointPort != 0 && su.Rank == 0 {
		if n, nerr := s.q.GetNode(ctx, nodeID); nerr == nil && n.Inventory.Valid {
			var inv inventory.Inventory
			if json.Unmarshal([]byte(n.Inventory.String), &inv) == nil {
				if addr := firstNonLoopback(&inv); addr != "" {
					endpoint = sql.NullString{String: net.JoinHostPort(addr, strconv.Itoa(int(su.EndpointPort))), Valid: true}
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
		if run, getErr := s.runs.Get(ctx, runID); getErr == nil && !runs.State(run.State).Terminal() {
			_ = s.runs.SetState(ctx, runID, runs.Cancelled, "", "")
		}
	}
	diagnostics := diag.Encode(diag.Compact(diag.Decode(row.Diagnostics)))
	if err := s.q.SetDeploymentStopping(ctx, db.SetDeploymentStoppingParams{
		Diagnostics: diagnostics,
		ID:          depID,
	}); err != nil {
		return
	}
	_ = s.runs.ReleaseLeasesFor(ctx, "deployment", depID)
	_ = s.q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{
		ObservedState: "stopped",
		Diagnostics:   diagnostics,
		ID:            depID,
	})
	s.bus.Publish(ctx, "deployment.stopped", depID, mustJSON(map[string]any{"deployment_id": depID}))
}

// ---------------------------------------------------------------- stop

// Stop drives every rank toward stopped. Offline ranks keep their leases and
// are re-driven by an explicit retry or reconnect until physically confirmed.
func (s *Service) Stop(ctx context.Context, depID string) (*Deployment, error) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, ErrUnknown
	}
	switch row.DesiredState {
	case "stopped":
		if row.ObservedState == "stopped" {
			return s.Get(ctx, depID)
		}
	case "running":
	default:
		return nil, fmt.Errorf("%w: deployment is %s", ErrState, row.DesiredState)
	}

	if err := s.q.SetDeploymentStopping(ctx, db.SetDeploymentStoppingParams{
		Diagnostics: row.Diagnostics,
		ID:          depID,
	}); err != nil {
		return nil, err
	}

	runID := ""
	if row.RunID.Valid {
		runID = row.RunID.String
	}
	if row.DesiredState == "running" && runID != "" {
		run, getErr := s.runs.Get(ctx, runID)
		if getErr != nil {
			return nil, getErr
		}
		if !runs.State(run.State).Terminal() {
			if err := s.runs.Cancel(ctx, runID); err != nil {
				return nil, err
			}
		}
	}

	placements := ParsePlacementSet(row.Placement)
	for _, rank := range placements.AllRanks() {
		if placement := placements.EntryFor(rank); placement != nil {
			s.dispatchNext(ctx, depID, rank, runID, *placement)
		}
	}
	s.checkStopComplete(ctx, depID, runID)
	s.bus.Publish(ctx, "deployment.stopping", depID, mustJSON(map[string]any{"deployment_id": depID}))
	return s.Get(ctx, depID)
}

// Start re-drives a fully-stopped deployment back to running, making the
// stop/start lifecycle reversible without losing the deployment's identity.
// It recomputes the live plan from the persisted placement, creates a fresh
// serve run, re-acquires the exact resource leases, resets every rank's
// dispatch phase to the beginning, and re-dispatches PULL->CREATE->START.
// The deployment must be observed `stopped`; a deployment still stopping is
// rejected.
func (s *Service) Start(ctx context.Context, depID string) (*Deployment, error) {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return nil, ErrUnknown
	}
	if row.DesiredState == "running" {
		return nil, fmt.Errorf("%w: deployment is already running", ErrState)
	}
	if row.ObservedState != "stopped" {
		return nil, fmt.Errorf("%w: deployment is %s (start requires observed stopped)", ErrState, row.ObservedState)
	}

	ps := ParsePlacementSet(row.Placement)
	var overrides []PlacementOverride
	for _, e := range ps.Entries {
		overrides = append(overrides, PlacementOverride{NodeID: e.NodeID, Rank: e.Rank})
	}
	plan, err := s.Plan(ctx, PlanRequest{
		RecipeDigest: row.RecipeDigest,
		Profile:      row.Profile,
		Placements:   overrides,
		Variants:     ps.Variants,
	})
	if err != nil {
		return nil, err
	}
	if !plan.Ready {
		return nil, fmt.Errorf("%w: %v", ErrNotReady, diag.Decode(diag.Encode(plan.Diagnostics)))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	qtx := db.New(tx)

	runIDStr, _ := id.New()
	input, _ := json.Marshal(map[string]any{
		"recipe_digest": plan.RecipeDigest,
		"profile":       plan.Profile,
		"plan_digest":   plan.Digest,
	})
	if err := qtx.CreateRun(ctx, db.CreateRunParams{
		ID: runIDStr, Module: "serving", Kind: "serve", State: "queued",
		Resources: "[]", Input: string(input),
		DeploymentID: sql.NullString{String: depID, Valid: true},
	}); err != nil {
		tx.Rollback()
		return nil, err
	}
	ps = placementSetFromPlan(plan)
	var fabric sql.NullString
	if plan.Fabric != nil {
		fabric = sql.NullString{String: *plan.Fabric, Valid: true}
	}
	var endpoint sql.NullString
	if plan.Endpoint.Host != "" {
		endpoint = sql.NullString{
			String: net.JoinHostPort(plan.Endpoint.Host, strconv.Itoa(int(plan.Endpoint.Port))),
			Valid:  true,
		}
	}
	if err := s.runs.AcquireLeases(ctx, qtx, "deployment", depID, plan.LeaseResources()); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if err := qtx.RestartDeployment(ctx, db.RestartDeploymentParams{
		RunID:         sql.NullString{String: runIDStr, Valid: true},
		Placement:     ps.Marshal(),
		Fabric:        fabric,
		Endpoint:      endpoint,
		EndpointModel: sql.NullString{String: plan.Endpoint.Model, Valid: plan.Endpoint.Model != ""},
		EndpointPath:  sql.NullString{String: plan.Endpoint.Path, Valid: plan.Endpoint.Path != ""},
		ID:            depID,
	}); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	s.bus.Publish(ctx, "deployment.started", depID, mustJSON(map[string]any{
		"deployment_id": depID, "run_id": runIDStr, "recipe": plan.RecipeName,
	}))
	_ = s.runs.SetState(ctx, runIDStr, runs.Planning, "", "")
	_ = s.runs.SetState(ctx, runIDStr, runs.Waiting, "", "")
	for _, pl := range plan.Placements {
		s.dispatchNext(ctx, depID, pl.Rank, runIDStr, pl)
	}
	return s.Get(ctx, depID)
}

// Delete removes a fully-stopped deployment and its runs, freeing the
// recipe/placement slot for a fresh deployment. A deployment that is
// running or still stopping is rejected.
func (s *Service) Delete(ctx context.Context, depID string) error {
	row, err := s.q.GetDeployment(ctx, depID)
	if err != nil {
		return ErrUnknown
	}
	if row.DesiredState != "stopped" || row.ObservedState != "stopped" {
		return fmt.Errorf("%w: deployment is %s/%s (delete requires stopped/stopped)", ErrState, row.DesiredState, row.ObservedState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	qtx := db.New(tx)
	if err := qtx.DeleteDeploymentRuns(ctx, sql.NullString{String: depID, Valid: true}); err != nil {
		tx.Rollback()
		return err
	}
	if err := qtx.DeleteDeployment(ctx, depID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bus.Publish(ctx, "deployment.deleted", depID, mustJSON(map[string]any{"deployment_id": depID}))
	return nil
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
		if host, _, splitErr := net.SplitHostPort(hostPort); splitErr == nil {
			hostPort = net.JoinHostPort(host, strconv.Itoa(int(probe.HTTPGet.Port)))
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
	existing = diag.Upsert(existing, d)
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
	Engine            string            `json:"engine,omitempty"`
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

// MonitorTarget is the narrow view the serving poller consumes: only the
// persisted endpoint metadata needed to probe a running deployment. It avoids
// the recipe re-parsing that Service.List performs for operator rendering.
type MonitorTarget struct {
	ID            string
	DesiredState  string
	ObservedState string
	Endpoint      Endpoint
	// EndpointModel/EndpointPath are the persisted identity; empty for
	// pre-migration rows that fell back to recipe/profile metadata.
	EndpointModel string
	EndpointPath  string
}

// ListMonitoringTargets returns the desired-running healthy/degraded
// deployments that carry a recorded endpoint, for the serving poller.
func (s *Service) ListMonitoringTargets(ctx context.Context) ([]MonitorTarget, error) {
	rows, err := s.q.ListDeploymentMonitorTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MonitorTarget, 0, len(rows))
	for _, r := range rows {
		t := MonitorTarget{
			ID:            r.ID,
			DesiredState:  r.DesiredState,
			ObservedState: r.ObservedState,
		}
		if r.Endpoint.Valid && r.Endpoint.String != "" {
			parts := strings.SplitN(r.Endpoint.String, ":", 2)
			if len(parts) == 2 {
				var port int32
				if _, err := fmt.Sscanf(parts[1], "%d", &port); err == nil {
					t.Endpoint = Endpoint{Host: parts[0], Port: port}
				}
			}
		}
		if r.EndpointModel.Valid {
			t.EndpointModel = r.EndpointModel.String
		}
		if r.EndpointPath.Valid {
			t.EndpointPath = r.EndpointPath.String
		}
		out = append(out, t)
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
		EndpointModel:     r.EndpointModel,
		EndpointPath:      r.EndpointPath,
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
		EndpointModel:     r.EndpointModel,
		EndpointPath:      r.EndpointPath,
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
	var endpointModel string
	if recipeRow, err := s.q.GetRecipe(ctx, row.RecipeDigest); err == nil {
		v.RecipeName = recipeRow.Name
		v.RecipeVersion = recipeRow.Version
		manifest, parseErr := recipe.Parse([]byte(recipeRow.Manifest))
		if parseErr != nil {
			return nil, fmt.Errorf("recipe manifest: %w", parseErr)
		}
		v.Engine = manifest.Metadata.Engine
		values, profileErr := manifest.ProfileValues(row.Profile)
		if profileErr != nil {
			return nil, fmt.Errorf("recipe profile: %w", profileErr)
		}
		endpointModel = profileString(values, "model")
		if endpointModel == "" {
			endpointModel = manifest.Metadata.Model
		}
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
				v.Endpoint = &Endpoint{Host: parts[0], Port: port, Model: endpointModel}
			}
		}
	}
	// Endpoint metadata (model/path) is persisted at create so it survives
	// restart; older deployments carry null and fall back at probe time.
	if row.EndpointModel.Valid || row.EndpointPath.Valid {
		if v.Endpoint == nil {
			v.Endpoint = &Endpoint{}
		}
		if row.EndpointModel.Valid {
			v.Endpoint.Model = row.EndpointModel.String
		}
		if row.EndpointPath.Valid {
			v.Endpoint.Path = row.EndpointPath.String
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
	nodeAddress := ""
	if node, nodeErr := s.q.GetNode(ctx, pl.NodeID); nodeErr == nil && node.Inventory.Valid {
		var inv inventory.Inventory
		if json.Unmarshal([]byte(node.Inventory.String), &inv) == nil {
			nodeAddress = firstNonLoopback(&inv)
		}
	}
	fabricAddress := ""
	if row.Fabric.Valid {
		if fabric, fabricErr := s.q.GetFabric(ctx, row.Fabric.String); fabricErr == nil && fabric.Address.Valid {
			fabricAddress = fabric.Address.String
		}
	}
	rctx := recipe.RenderContext{
		NodeID: pl.NodeID, NodeRank: int(rank), NodeAddress: nodeAddress,
		FabricAddr: fabricAddress, Artifacts: map[string]string{}, Profiles: values,
	}
	for _, artifact := range m.Artifacts {
		dest := artifact.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + artifact.Name
		}
		renderedPath := dest
		identity, identityErr := canonicalArtifactIdentity(artifact, variantsFor(row)[artifact.Name])
		if identityErr != nil {
			return nil, identityErr
		}
		if parsed, parseErr := artifactidentity.Parse(identity); parseErr == nil && parsed.Revision != "" {
			renderedPath = filepath.ToSlash(filepath.Join(dest, "snapshots", parsed.Revision))
		}
		rctx.Artifacts[artifact.Name] = renderedPath
	}

	permissions := map[string]bool{}
	for _, permission := range w.Permissions {
		permissions[permission] = true
	}
	if w.Resources.CPU <= 0 || w.Resources.MemoryBytes <= 0 || w.Resources.Pids <= 0 {
		return nil, fmt.Errorf("workload resources must set positive cpu, memoryBytes, and pids")
	}
	networkMode := w.NetworkMode
	if networkMode == "" {
		networkMode = "none"
	}
	if networkMode == "host" && !permissions["network.host"] {
		return nil, fmt.Errorf("host networking requires network.host permission")
	}
	spec := &runtime.ContainerSpec{
		Name: containerName(depID, runID, rank), Image: w.Image.Reference, ImageDigest: w.Image.Digest,
		Entrypoint: w.Command, NetworkMode: networkMode,
		ReadonlyRootfs: !permissions["rootfs.write"], NoNewPrivileges: true, CapDrop: []string{"ALL"},
	}
	for _, argument := range w.Args {
		rendered, renderErr := m.Render(argument, rctx)
		if renderErr != nil {
			return nil, renderErr
		}
		spec.Cmd = append(spec.Cmd, rendered)
	}
	for key, value := range w.Env {
		rendered, renderErr := m.Render(value, rctx)
		if renderErr != nil {
			return nil, renderErr
		}
		spec.Env = append(spec.Env, key+"="+rendered)
	}
	sort.Strings(spec.Env)
	spec.CPU = w.Resources.CPU
	spec.MemoryBytes = w.Resources.MemoryBytes
	spec.ShmBytes = w.Resources.ShmBytes
	spec.TmpfsBytes = w.Resources.TmpfsBytes
	spec.PidsLimit = w.Resources.Pids
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
	// The workload drops all capabilities (CapDrop ALL), so it has no
	// CAP_IPC_LOCK. RoCE/NCCL registers GPU/IB buffers via ibv_reg_mr, which
	// needs RLIMIT_MEMLOCK; the default 8 KiB hard limit makes
	// ibv_reg_mr_iova2 fail with ENOMEM and aborts NCCL connect. Match the
	// prior Spark launcher: memlock unlimited + 64 MiB stack.
	if len(spec.RDMAPaths) > 0 {
		spec.Ulimits = []runtime.Ulimit{
			{Name: "memlock", Hard: -1, Soft: -1},
			{Name: "stack", Hard: 67108864, Soft: 67108864},
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
	packageArtifact, packageErr := s.q.GetArtifactByIdentity(ctx, "recipe://"+row.RecipeDigest)
	if packageErr != nil {
		return nil, fmt.Errorf("recipe package: %w", packageErr)
	}
	packagePath, packageOK := s.validPlacement(ctx, packageArtifact.ID, pl.NodeID)
	if !packageOK {
		return nil, fmt.Errorf("recipe package: no valid placement on node %s", pl.NodeID)
	}
	spec.Mounts = append(spec.Mounts, runtime.MountSpec{
		Source: filepath.Join(packagePath, "assets"), Dest: "/lmw/assets", ReadOnly: true,
	})
	for _, a := range m.Artifacts {
		identity, err := canonicalArtifactIdentity(a, variantsFor(row)[a.Name])
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", a.Name, err)
		}
		dest := a.Mount
		if dest == "" {
			dest = "/var/lib/lmw/artifacts/" + a.Name
		}
		art, aerr := s.q.GetArtifactByIdentity(ctx, identity)
		if aerr != nil {
			return nil, fmt.Errorf("artifact %s: unknown identity %s", a.Name, identity)
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
	if spec.ShmBytes > 64<<20 && !permissions["memory.shm-large"] {
		return nil, fmt.Errorf("large shared memory requires memory.shm-large permission")
	}
	if len(spec.RDMAPaths) > 0 && !permissions["devices.rdma"] {
		return nil, fmt.Errorf("RDMA devices require devices.rdma permission")
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
