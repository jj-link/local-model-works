// Package fabric owns multi-node transport groups: membership validation
// against node inventories, optimistic-concurrency CRUD, and state
// revalidation when member inventories or statuses change.
package fabric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/inventory"
)

// Transport names.
const (
	TransportRoCE = "roce"
	TransportTCP  = "tcp"
)

// Sentinel errors; the API layer maps these to 404/409/422.
var (
	ErrUnknown         = errors.New("unknown fabric")
	ErrVersionMismatch = errors.New("fabric version mismatch")
	ErrNameConflict    = errors.New("fabric name conflict")
	ErrInUse           = errors.New("fabric in use")
	ErrValidation      = errors.New("fabric validation failed")
)

// MemberBinding is the rank-ordered, node-specific transport wiring. Spark
// pairs commonly use asymmetric interface and RDMA device names.
type MemberBinding struct {
	NodeID        string `json:"node_id"`
	InterfaceName string `json:"interface_name"`
	Address       string `json:"address"`
	RDMADevice    string `json:"rdma_device,omitempty"`
	GIDIndex      *int32 `json:"gid_index,omitempty"`
}

// Fabric is the API view of one transport group.
type Fabric struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Transport   string            `json:"transport"`
	Members     []string          `json:"members"`
	Bindings    []MemberBinding   `json:"bindings"`
	State       string            `json:"state"`
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
	Version     string            `json:"version"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
}

// CreateRequest is the operator-supplied group definition. Name is
// immutable after creation. Bindings follow Members in the same rank order.
type CreateRequest struct {
	Name      string          `json:"name"`
	Transport string          `json:"transport"`
	Members   []string        `json:"members"`
	Bindings  []MemberBinding `json:"bindings"`
}

// Service validates and persists fabrics.
type Service struct {
	q   *db.Queries
	bus *events.EventBus
}

func New(q *db.Queries, bus *events.EventBus) *Service {
	return &Service{q: q, bus: bus}
}

// Validate computes the fabric state and diagnostics for a candidate
// group definition against the current node set. excludeID skips one
// fabric row (self) in the name-uniqueness check.
func (s *Service) Validate(ctx context.Context, excludeID string, req CreateRequest) (string, []diag.Diagnostic, error) {
	var ds []diag.Diagnostic
	name := req.Name
	if name == "" {
		ds = append(ds, diag.Error("fabric.invalid_name", "name is required"))
	} else if stringsContainAny(name, "/\\") || name == "." || name == ".." {
		ds = append(ds, diag.Error("fabric.invalid_name", "name must not contain path separators"))
	} else {
		rows, err := s.q.ListFabrics(ctx)
		if err != nil {
			return "", nil, err
		}
		for _, row := range rows {
			if row.Name == name && row.ID != excludeID {
				ds = append(ds, diag.Error("fabric.name_conflict", fmt.Sprintf("fabric %q already exists (id %s)", name, row.ID)))
				break
			}
		}
	}
	switch req.Transport {
	case TransportRoCE, TransportTCP:
	default:
		ds = append(ds, diag.Error("fabric.unknown_transport", fmt.Sprintf("transport must be %q or %q", TransportRoCE, TransportTCP)))
	}
	if len(req.Members) < 2 {
		ds = append(ds, diag.Error("fabric.too_few_members", "at least two members are required"))
	}
	if len(req.Bindings) != len(req.Members) {
		ds = append(ds, diag.Error("fabric.bindings_incomplete", "every ordered member requires one transport binding"))
	}

	seen := map[string]bool{}
	nodes, err := s.nodeSet(ctx)
	if err != nil {
		return "", nil, err
	}
	inventoryReady := true
	for index, nodeID := range req.Members {
		if seen[nodeID] {
			ds = append(ds, diag.Error("fabric.duplicate_member", nodeID))
		}
		seen[nodeID] = true
		node, ok := nodes[nodeID]
		if !ok {
			ds = append(ds, diag.Error("fabric.unknown_member", nodeID))
			continue
		}
		if node.status == "pending" {
			inventoryReady = false
			ds = append(ds, diag.Warning("fabric.member_unapproved", fmt.Sprintf("member %s is awaiting approval", nodeID)))
		}
		if node.status == "offline" {
			ds = append(ds, diag.Warning("fabric.member_offline", fmt.Sprintf("member %s is offline", nodeID)))
		}

		var inv *inventory.Inventory
		if node.inventory.Valid && node.inventory.String != "" {
			inv, err = inventory.Parse(node.inventory.String)
			if err != nil {
				inventoryReady = false
				ds = append(ds, diag.Warning("fabric.inventory_unreadable", fmt.Sprintf("member %s inventory: %v", nodeID, err)))
			}
		} else {
			inventoryReady = false
			ds = append(ds, diag.Warning("fabric.no_inventory", fmt.Sprintf("member %s has no inventory", nodeID)))
		}

		if index >= len(req.Bindings) {
			continue
		}
		binding := req.Bindings[index]
		if binding.NodeID != nodeID {
			ds = append(ds, diag.Error("fabric.binding_order", fmt.Sprintf("binding %d names %s, want ordered member %s", index, binding.NodeID, nodeID)))
			continue
		}
		if binding.InterfaceName == "" {
			ds = append(ds, diag.Error("fabric.interface_required", fmt.Sprintf("member %s requires a network interface", nodeID)))
		} else if inv != nil && !inv.HasInterface(binding.InterfaceName) {
			ds = append(ds, diag.Error("fabric.member_no_interface", fmt.Sprintf("member %s lacks interface %s", nodeID, binding.InterfaceName)))
		}
		if binding.Address == "" {
			ds = append(ds, diag.Error("fabric.address_required", fmt.Sprintf("member %s requires a fabric address", nodeID)))
		} else if inv != nil && binding.InterfaceName != "" {
			found := false
			for _, address := range inv.InterfaceAddresses(binding.InterfaceName) {
				if address == binding.Address {
					found = true
					break
				}
			}
			if !found {
				ds = append(ds, diag.Error("fabric.address_mismatch",
					fmt.Sprintf("address %s is not assigned to member %s interface %s", binding.Address, nodeID, binding.InterfaceName)))
			}
		}
		if req.Transport == TransportRoCE {
			if binding.RDMADevice == "" {
				ds = append(ds, diag.Error("fabric.roce_requires_device", fmt.Sprintf("member %s requires an RDMA device", nodeID)))
			} else if inv != nil {
				device := inv.RDMADevice(binding.RDMADevice)
				if device == nil {
					ds = append(ds, diag.Error("fabric.member_no_rdma", fmt.Sprintf("member %s lacks device %s", nodeID, binding.RDMADevice)))
				} else {
					if len(device.NetworkInterfaces) > 0 {
						associated := false
						for _, interfaceName := range device.NetworkInterfaces {
							associated = associated || interfaceName == binding.InterfaceName
						}
						if !associated {
							ds = append(ds, diag.Error("fabric.rdma_interface_mismatch",
								fmt.Sprintf("device %s on member %s is not attached to interface %s", binding.RDMADevice, nodeID, binding.InterfaceName)))
						}
					}
					if binding.GIDIndex != nil {
						reported, populated := false, false
						for _, port := range device.Ports {
							for _, gid := range port.GIDs {
								reported = true
								if gid.Index == *binding.GIDIndex && strings.Trim(gid.Value, ":0") != "" {
									populated = true
								}
							}
						}
						if !reported {
							ds = append(ds, diag.Error("fabric.gids_unreported", fmt.Sprintf("member %s has not reported its RDMA GID table", nodeID)))
						} else if !populated {
							ds = append(ds, diag.Error("fabric.gid_unavailable",
								fmt.Sprintf("GID index %d is empty or absent on member %s device %s", *binding.GIDIndex, nodeID, binding.RDMADevice)))
						}
					}
				}
			}
			if binding.GIDIndex == nil || *binding.GIDIndex < 0 || *binding.GIDIndex > 255 {
				ds = append(ds, diag.Error("fabric.gid_index_required", fmt.Sprintf("member %s requires a GID index from 0 to 255", nodeID)))
			}
		}
	}
	state := "ok"
	if diag.HasError(ds) || !inventoryReady {
		state = "incomplete"
	}
	return state, ds, nil
}

// Create inserts a new fabric with computed state.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Fabric, error) {
	state, ds, err := s.Validate(ctx, "", req)
	if err != nil {
		return Fabric{}, err
	}
	if diag.HasError(ds) {
		return Fabric{}, fmt.Errorf("%w: %s", ErrValidation, ds[0].Message)
	}
	fid, err := id.New()
	if err != nil {
		return Fabric{}, err
	}
	ver, err := id.New()
	if err != nil {
		return Fabric{}, err
	}
	members, err := json.Marshal(req.Members)
	if err != nil {
		return Fabric{}, err
	}
	bindings, err := json.Marshal(req.Bindings)
	if err != nil {
		return Fabric{}, err
	}
	if err := s.q.CreateFabric(ctx, db.CreateFabricParams{
		ID: fid, Name: req.Name, Transport: req.Transport,
		InterfaceName: sql.NullString{}, Address: sql.NullString{}, RdmaDevice: sql.NullString{},
		Members: string(members), Bindings: string(bindings), Version: ver,
	}); err != nil {
		return Fabric{}, err
	}
	if err := s.q.UpdateFabricState(ctx, db.UpdateFabricStateParams{
		State: state, Diagnostics: diag.Encode(ds), ID: fid,
	}); err != nil {
		return Fabric{}, err
	}
	s.publishChanged(ctx, fid, req.Name, state)
	return s.Get(ctx, fid)
}

// Update re-validates and rewrites one fabric under If-Match. Name is
// immutable: req.Name must equal the stored name.
func (s *Service) Update(ctx context.Context, fid, ifMatch string, req CreateRequest) (Fabric, error) {
	if ifMatch == "" {
		return Fabric{}, errors.New("If-Match is required")
	}
	cur, err := s.q.GetFabricByIfMatch(ctx, db.GetFabricByIfMatchParams{ID: fid, Version: ifMatch})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Fabric{}, ErrVersionMismatch
		}
		return Fabric{}, err
	}
	if req.Name != cur.Name {
		return Fabric{}, fmt.Errorf("%w: name is immutable", ErrValidation)
	}
	state, ds, err := s.Validate(ctx, fid, req)
	if err != nil {
		return Fabric{}, err
	}
	if diag.HasError(ds) {
		return Fabric{}, fmt.Errorf("%w: %s", ErrValidation, ds[0].Message)
	}
	ver, err := id.New()
	if err != nil {
		return Fabric{}, err
	}
	members, _ := json.Marshal(req.Members)
	bindings, _ := json.Marshal(req.Bindings)
	if err := s.q.UpdateFabric(ctx, db.UpdateFabricParams{
		Transport: req.Transport, InterfaceName: sql.NullString{},
		Address: sql.NullString{}, RdmaDevice: sql.NullString{},
		Members: string(members), Bindings: string(bindings),
		State: state, Diagnostics: diag.Encode(ds),
		Version: ver, ID: cur.ID, Version_2: ifMatch,
	}); err != nil {
		return Fabric{}, err
	}
	s.publishChanged(ctx, fid, req.Name, state)
	return s.Get(ctx, fid)
}

// Delete removes a fabric that no running deployment references.
// ifMatch must carry the current version.
func (s *Service) Delete(ctx context.Context, fid, ifMatch string) error {
	cur, err := s.q.GetFabric(ctx, fid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknown
		}
		return err
	}
	if ifMatch != "" && ifMatch != cur.Version {
		return ErrVersionMismatch
	}
	refs, err := s.q.CountActiveDeploymentsOnFabric(ctx, nullStr(fid))
	if err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: %d running deployment(s) reference this fabric", ErrInUse, refs)
	}
	if err := s.q.DeleteFabric(ctx, db.DeleteFabricParams{ID: fid, Version: cur.Version}); err != nil {
		return err
	}
	s.bus.Publish(ctx, "fabric.deleted", "", mustJSON(map[string]any{"id": fid}))
	return nil
}

// Get returns one fabric.
func (s *Service) Get(ctx context.Context, fid string) (Fabric, error) {
	row, err := s.q.GetFabric(ctx, fid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Fabric{}, ErrUnknown
		}
		return Fabric{}, err
	}
	return render(row), nil
}

// List returns all fabrics by name.
func (s *Service) List(ctx context.Context) ([]Fabric, error) {
	rows, err := s.q.ListFabrics(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Fabric, 0, len(rows))
	for _, r := range rows {
		out = append(out, render(r))
	}
	return out, nil
}

// RevalidateNode recomputes state for every fabric containing nodeID
// after an inventory or status change.
func (s *Service) RevalidateNode(ctx context.Context, nodeID string) error {
	rows, err := s.q.ListFabrics(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		var members []string
		if err := json.Unmarshal([]byte(r.Members), &members); err != nil {
			continue
		}
		contains := false
		for _, m := range members {
			if m == nodeID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		}
		state, ds, err := s.Validate(ctx, r.ID, CreateRequest{
			Name: r.Name, Transport: r.Transport, Members: members, Bindings: ParseBindings(r.Bindings),
		})
		if err != nil {
			return err
		}
		if err := s.q.UpdateFabricState(ctx, db.UpdateFabricStateParams{
			State: state, Diagnostics: diag.Encode(ds), ID: r.ID,
		}); err != nil {
			return err
		}
		s.publishChanged(ctx, r.ID, r.Name, state)
	}
	return nil
}

type nodeInfo struct {
	status    string
	inventory sql.NullString
}

func (s *Service) nodeSet(ctx context.Context) (map[string]*nodeInfo, error) {
	rows, err := s.q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*nodeInfo, len(rows))
	for i := range rows {
		out[rows[i].ID] = &nodeInfo{status: rows[i].Status, inventory: rows[i].Inventory}
	}
	return out, nil
}

func render(row db.Fabric) Fabric {
	var members []string
	_ = json.Unmarshal([]byte(row.Members), &members)
	return Fabric{
		ID: row.ID, Name: row.Name, Transport: row.Transport, Members: members,
		Bindings: ParseBindings(row.Bindings),
		State:    row.State, Diagnostics: diag.Decode(row.Diagnostics), Version: row.Version,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// ParseBindings decodes the persisted member wiring. Malformed data renders
// as incomplete instead of leaking partially trusted values into workloads.
func ParseBindings(raw string) []MemberBinding {
	bindings := []MemberBinding{}
	if err := json.Unmarshal([]byte(raw), &bindings); err != nil {
		return []MemberBinding{}
	}
	return bindings
}

func (s *Service) publishChanged(ctx context.Context, fid, name, state string) {
	_ = s.bus.Publish(ctx, "fabric.changed", "", mustJSON(map[string]any{"id": fid, "name": name, "state": state}))
}

func nullStr(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nullStrValue(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func stringsContainAny(s, chars string) bool {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(chars); j++ {
			if s[i] == chars[j] {
				return true
			}
		}
	}
	return false
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
