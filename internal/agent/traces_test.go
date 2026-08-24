package agent

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/traces"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestTraceSpoolRedactsBeforeDiskAndDeletesAfterFinalAck(t *testing.T) {
	agent := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", nil, nil)
	spec := &runtime.ContainerSpec{}
	command := &agentv1.WorkloadCommand{RunId: "run-1", Rank: 0, TraceEnabled: true, TraceSchema: traces.SchemaVersion, TraceSocket: traceSocketPath, Secrets: map[string]string{"model-provider": "super-secret-value"}}
	if err := agent.traceSpools.start(command, spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].Dest != "/run/lmw" {
		t.Fatalf("trace mount = %+v", spec.Mounts)
	}
	conn, err := net.Dial("unix", agent.traceSpools.spools[traceSpoolKey("run-1", 0, traceSource)].socket)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	credentials, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(credentials, "super-secret-value") {
		t.Fatalf("credential handshake = %q, err=%v", credentials, err)
	}
	event := map[string]any{"sequence": 0, "event_id": "event-0", "occurred_at": "2026-08-24T00:00:00Z", "kind": "message", "payload": map[string]any{"content": "token super-secret-value"}}
	if err := json.NewEncoder(conn).Encode(event); err != nil {
		t.Fatal(err)
	}
	var chunk *agentv1.TraceChunk
	select {
	case message := <-agent.sendQ:
		chunk = message.GetTraceChunk()
	case <-time.After(5 * time.Second):
		t.Fatal("trace chunk not sent")
	}
	if chunk == nil || strings.Contains(string(chunk.Data), "super-secret-value") || !strings.Contains(string(chunk.Data), "[REDACTED:stored-secret]") {
		t.Fatalf("unsafe trace chunk: %q", chunk.GetData())
	}
	spool := agent.traceSpools.spools[traceSpoolKey("run-1", 0, traceSource)]
	disk, err := os.ReadFile(spool.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(disk), "super-secret-value") {
		t.Fatalf("secret reached spool: %s", disk)
	}
	agent.traceSpools.ack(&agentv1.TraceAck{RunId: "run-1", Rank: 0, Source: traceSource, CommittedOffset: uint64(len(chunk.Data))})
	if err := agent.traceSpools.finalize("run-1", 0); err != nil {
		t.Fatal(err)
	}
	var final *agentv1.TraceChunk
	select {
	case message := <-agent.sendQ:
		final = message.GetTraceChunk()
	case <-time.After(5 * time.Second):
		t.Fatal("final trace marker not sent")
	}
	if final == nil || !final.Final {
		t.Fatalf("final chunk = %+v", final)
	}
	agent.traceSpools.ack(&agentv1.TraceAck{RunId: "run-1", Rank: 0, Source: traceSource, CommittedOffset: final.EndOffset, Final: true})
	if agent.traceSpools.enabled("run-1", 0) {
		t.Fatal("acknowledged spool retained")
	}
	conn.Close()
}
