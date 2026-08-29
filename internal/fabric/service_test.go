package fabric

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/inventory"
)

func TestPerMemberBindingsValidateAsymmetricSparkFabric(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "fabric.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	queries := db.New(database)
	service := New(queries, events.NewEventBus(queries))

	seedFabricNode(t, queries, "spark2", "enp1s0f1np1", "10.0.0.1", "rocep1s0f1")
	seedFabricNode(t, queries, "spark3", "enp1s0f0np0", "10.0.0.2", "rocep1s0f0")
	gid3, gid2 := int32(3), int32(2)
	request := CreateRequest{
		Name: "spark-p2p", Transport: TransportRoCE,
		Members: []string{"spark2", "spark3"},
		Bindings: []MemberBinding{
			{NodeID: "spark2", InterfaceName: "enp1s0f1np1", Address: "10.0.0.1", RDMADevice: "rocep1s0f1", GIDIndex: &gid3},
			{NodeID: "spark3", InterfaceName: "enp1s0f0np0", Address: "10.0.0.2", RDMADevice: "rocep1s0f0", GIDIndex: &gid2},
		},
	}

	state, diagnostics, err := service.Validate(ctx, "", request)
	if err != nil || state != "ok" || diag.HasError(diagnostics) {
		t.Fatalf("valid asymmetric fabric: state=%s diagnostics=%+v err=%v", state, diagnostics, err)
	}
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Bindings) != 2 || created.Bindings[1].RDMADevice != "rocep1s0f0" || *created.Bindings[1].GIDIndex != gid2 {
		t.Fatalf("persisted bindings = %+v", created.Bindings)
	}

	bad := request
	bad.Name = "spark-bad"
	bad.Bindings = append([]MemberBinding(nil), request.Bindings...)
	bad.Bindings[1].RDMADevice = "rocep1s0f1"
	state, diagnostics, err = service.Validate(ctx, "", bad)
	if err != nil || state != "incomplete" || !diag.HasError(diagnostics) {
		t.Fatalf("invalid shared RDMA name: state=%s diagnostics=%+v err=%v", state, diagnostics, err)
	}
}

func seedFabricNode(t *testing.T, queries *db.Queries, nodeID, interfaceName, address, rdmaDevice string) {
	t.Helper()
	ctx := context.Background()
	if err := queries.CreateNode(ctx, db.CreateNodeParams{ID: nodeID, DisplayName: nodeID, Labels: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "online", ID: nodeID}); err != nil {
		t.Fatal(err)
	}
	report, err := json.Marshal(inventory.Inventory{
		Interfaces: []inventory.Interface{{Name: interfaceName, Addresses: []string{address}}},
		RdmaDevices: []inventory.RdmaDevice{{
			Name: rdmaDevice, NetworkInterfaces: []string{interfaceName},
			Ports: []inventory.RdmaPort{{
				Name: "1", State: "active", LinkRateGbps: 200,
				GIDs: []inventory.RdmaGID{
					{Index: 2, Value: "0000:0000:0000:0000:0000:ffff:0a00:0002"},
					{Index: 3, Value: "0000:0000:0000:0000:0000:ffff:0a00:0001"},
				},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetNodeInventory(ctx, db.SetNodeInventoryParams{
		Inventory: sql.NullString{String: string(report), Valid: true}, ID: nodeID,
	}); err != nil {
		t.Fatal(err)
	}
}
