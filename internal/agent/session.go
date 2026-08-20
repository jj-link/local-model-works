package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"connectrpc.com/connect"

	"github.com/jj-link/local-model-works/internal/hardware"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// bidi is the control-channel stream type (connect bidirectional client).
type bidi = *connect.BidiStreamForClient[agentv1.AgentMessage, agentv1.ServerMessage]

// errCleanClose signals the controller asked for the session to end.
var errCleanClose = errors.New("session closed by controller")

// heartbeatPeriod is how often the agent reports liveness.
const heartbeatPeriod = 5 * time.Second

// session runs one control channel: it reports inventory and placements,
// sends heartbeats/telemetry, drains the event queue, and receives workload
// commands, log requests, transfers, reconciliation, and certificate
// rotation.
func (a *Agent) session(ctx context.Context) error {
	client := a.sessionClient(ctx)
	if client == nil {
		return errors.New("no controller transport")
	}
	stream := client.Session(ctx)
	defer stream.CloseResponse()
	// The select below only observes ctx between messages; a cancel that
	// lands mid-Receive would leave the HTTP/2 stream open on both ends
	// and hang the session (and Agent.Run) forever. Closing the response
	// side on cancel resets the stream and wakes a pending Receive.
	go func() {
		<-ctx.Done()
		_ = stream.CloseResponse()
	}()

	// Report what this node holds and needs.
	a.refreshDocker()
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Inventory{
		Inventory: a.probeInventory(),
	}})
	a.reportPlacements(ctx)
	if a.certRemaining() < RotationThreshold {
		a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_RotateCertificate{
			RotateCertificate: &agentv1.RotateCertificateRequest{CurrentSerial: a.currentSerial()},
		}})
	}
	a.reconcile(ctx, nil)

	errCh := make(chan error, 1)
	go func() { errCh <- a.sendLoop(ctx, stream) }()

	for {
		select {
		case <-ctx.Done():
			stream.CloseRequest()
			<-errCh
			return ctx.Err()
		case err := <-errCh:
			return err
		default:
		}
		res, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errCleanClose
			}
			return err
		}
		switch b := res.GetBody().(type) {
		case *agentv1.ServerMessage_HeartbeatAck:
			// Liveness confirmation; telemetry cadence is local.
		case *agentv1.ServerMessage_WorkloadCommand:
			go a.handleWorkload(ctx, b.WorkloadCommand)
		case *agentv1.ServerMessage_LogRequest:
			a.handleLogRequest(ctx, b.LogRequest)
		case *agentv1.ServerMessage_TransferCommand:
			go a.handleTransfer(ctx, b.TransferCommand)
		case *agentv1.ServerMessage_ArtifactCommand:
			go a.handleArtifact(ctx, b.ArtifactCommand)
		case *agentv1.ServerMessage_ExtensionCommand:
			go a.handleExtension(ctx, b.ExtensionCommand)
		case *agentv1.ServerMessage_ReconcileRequest:
			a.noteReconcile(b.ReconcileRequest.GetReason())
			if b.ReconcileRequest.GetReason() == "artifact.rescan" {
				a.reportPlacements(ctx)
			} else {
				a.reconcile(ctx, b.ReconcileRequest)
			}
		case *agentv1.ServerMessage_Certificate:
			if err := a.applyCertificate(b.Certificate); err != nil {
				log.Printf("agent: apply certificate: %v", err)
			}
		case *agentv1.ServerMessage_Close:
			return errCleanClose
		}
	}
}

// sendLoop is the only writer on the stream: periodic heartbeats and
// telemetry interleaved with event-driven messages.
func (a *Agent) sendLoop(ctx context.Context, stream bidi) error {
	hb := time.NewTicker(heartbeatPeriod)
	defer hb.Stop()
	tele := time.NewTicker(a.cfg.TelemetryInt)
	defer tele.Stop()

	if err := a.sendHeartbeat(stream); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb.C:
			if err := a.sendHeartbeat(stream); err != nil {
				return err
			}
		case <-tele.C:
			t, err := a.acc.Sample(ctx)
			if err != nil {
				t = hardware.Telemetry{}
			}
			a.sendTelemetry(t)
		case m := <-a.sendQ:
			if err := stream.Send(m); err != nil {
				return fmt.Errorf("send: %w", err)
			}
		}
	}
}

func (a *Agent) sendHeartbeat(stream bidi) error {
	uptime := time.Since(a.startTime)
	a.mu.Lock()
	dockerOK := a.dockerOK
	dockerVersion := a.dockerVersion
	a.mu.Unlock()
	return stream.Send(&agentv1.AgentMessage{
		Body: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{
				AgentVersion:  a.version,
				UptimeSeconds: uint64(uptime.Seconds()),
				DockerOk:      dockerOK,
				DockerVersion: dockerVersion,
			},
		},
	})
}

// sendTelemetry enqueues one telemetry sample (the send loop serializes
// writes).
func (a *Agent) sendTelemetry(t hardware.Telemetry) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_Telemetry{
		Telemetry: toProtoTelemetry(t),
	}})
}

// probeInventory composes the full node report: host baseline, accelerators
// (NVML, graceful when absent), Docker state, and configured cache roots.
func (a *Agent) probeInventory() *agentv1.Inventory {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inv := hardware.HostBaseline(ctx)
	inv.PeerListen = a.cfg.PeerAdvertiseAddr()
	if accInv, err := a.acc.Probe(ctx); err == nil {
		inv.Accelerators = accInv.Accelerators
		// Driver-reported network and RDMA entries supplement the host
		// baseline; the driver is the authoritative hardware source for
		// vendors that expose them. Host-reported entries win on name
		// collisions.
		for _, ni := range accInv.Interfaces {
			if !interfaceByName(inv.Interfaces, ni.Name) {
				inv.Interfaces = append(inv.Interfaces, ni)
			}
		}
		for _, d := range accInv.RDMADevices {
			if !rdmaByName(inv.RDMADevices, d.Name) {
				inv.RDMADevices = append(inv.RDMADevices, d)
			}
		}
	}
	a.mu.Lock()
	inv.Docker = hardware.DockerInfo{OK: a.dockerOK, Version: a.dockerVersion}
	a.mu.Unlock()
	for _, r := range a.cfg.CacheRoots {
		inv.CacheRoots = append(inv.CacheRoots, scanCacheRoot(ctx, r))
	}
	return toProtoInventory(inv)
}

func interfaceByName(list []hardware.NetworkInterface, name string) bool {
	for _, i := range list {
		if i.Name == name {
			return true
		}
	}
	return false
}

func rdmaByName(list []hardware.RdmaDevice, name string) bool {
	for _, d := range list {
		if d.Name == name {
			return true
		}
	}
	return false
}

// currentSerial is the hex serial of the live node certificate.
func (a *Agent) currentSerial() string {
	a.certMu.RLock()
	pair := a.nodeCert
	a.certMu.RUnlock()
	if pair == nil || len(pair.Certificate) == 0 {
		return ""
	}
	serial, err := serialOfPEM(concatPEM(pair.Certificate))
	if err != nil {
		return ""
	}
	return serial
}

// refreshDocker pings the container runtime and caches its state for
// heartbeats and inventory.
func (a *Agent) refreshDocker() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, err := a.rt.Ping(ctx); err == nil {
		a.mu.Lock()
		a.dockerOK = true
		a.dockerVersion = v
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	a.dockerOK = false
	a.dockerVersion = ""
	a.mu.Unlock()
}
