package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type retainedCaptureRuntime struct {
	runtime.Runtime
	mu      sync.Mutex
	batches [][]runtime.LogRecord
	opened  chan struct{}
}

func (r *retainedCaptureRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) runtime.LogCursor, emit func(runtime.LogRecord) error) error {
	r.mu.Lock()
	records := r.batches[0]
	r.batches = r.batches[1:]
	r.mu.Unlock()
	for _, record := range records {
		if cursor(record.Source).Offset >= record.Cursor.Offset {
			continue
		}
		if err := emit(record); err != nil {
			return err
		}
	}
	r.opened <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func captureRecord(source, stream, text string, offset int64) runtime.LogRecord {
	return runtime.LogRecord{Source: source, Stream: stream, Data: []byte(text), Cursor: runtime.LogCursor{Offset: offset}}
}

func TestCaptureRestartResumesSourcesDespiteReorderedHistory(t *testing.T) {
	first := captureRecord("supervisor", "stdout", "supervisor\n", 11)
	second := captureRecord("docker:original", "stdout", "engine\n", 7)
	fresh := captureRecord("docker:original", "stdout", "fresh-without-newline", 28)
	replacement := captureRecord("docker:replacement", "stdout", "engine\n", 7)
	rt := &retainedCaptureRuntime{opened: make(chan struct{}, 3), batches: [][]runtime.LogRecord{
		{first, second}, {second, first, fresh}, {replacement, first, second, fresh},
	}}
	cfg := config.Agent{StateRoot: t.TempDir()}
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234"}
	want := ""
	for generation, suffix := range []string{"supervisor\nengine\n", "fresh-without-newline", "engine\n"} {
		// A new Agent, not just a new reader, proves the cursors are durable.
		a := New(cfg, "test", "test", rt, nil)
		cleanupLogTailers(t, a)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		a.workloads.startTailer(ctx, key.runID, key.deploymentID, key.rank, "logical-installation")
		a.workloads.mu.Lock()
		done := a.workloads.tailers[key].done
		a.workloads.mu.Unlock()
		select {
		case <-rt.opened:
		case <-time.After(2 * time.Second):
			t.Logf("capture generation %d", generation)
			failLogCaptureWait(t, a, key, "source capture did not open")
		}
		awaitLogChunk(t, a, "stdout", suffix, uint64(len(want)))
		cancel()
		<-done
		want += suffix
		got, err := os.ReadFile(a.logPath(key, "stdout"))
		if err != nil || string(got) != want {
			t.Fatalf("generation %d: retained output %q, %v; want %q", generation, got, err, want)
		}
	}
}

func TestCapturePreservesLongPartialAndCarriageReturnBytes(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2*1024*1024)
	data = append(data, []byte("\rprogress\r\npartial")...)
	rt := &retainedCaptureRuntime{opened: make(chan struct{}, 1), batches: [][]runtime.LogRecord{{
		{Source: "engine", Stream: "stderr", Data: data, Cursor: runtime.LogCursor{Offset: int64(len(data))}},
	}}}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.workloads.startTailer(ctx, key.runID, key.deploymentID, key.rank, "engine")
	a.workloads.mu.Lock()
	done := a.workloads.tailers[key].done
	a.workloads.mu.Unlock()
	defer func() { cancel(); <-done }()
	var received []byte
	for len(received) < len(data) {
		select {
		case message := <-a.sendQ:
			chunk := message.GetLogChunk()
			if chunk == nil || chunk.Stream != "stderr" || chunk.Offset != uint64(len(received)) || chunk.Final {
				t.Fatalf("unexpected output event: %+v", message)
			}
			received = append(received, chunk.Data...)
		case <-time.After(2 * time.Second):
			t.Logf("received %d of %d original bytes", len(received), len(data))
			failLogCaptureWait(t, a, key, "long or unterminated output stalled")
		}
	}
	if !bytes.Equal(received, data) {
		t.Fatal("capture altered line endings or partial output")
	}
}

func TestCaptureRollsBackUncommittedAppendBeforeResume(t *testing.T) {
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", nil, nil)
	tailer := &tailer{a: a, key: tailKey{runID: "run-1234", deploymentID: "deployment-1234"}}
	c, err := openLogCapture(tailer)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.emit(captureRecord("source", "stdout", "committed", 9)); err != nil {
		t.Fatal(err)
	}
	if err := c.flush(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := c.emit(captureRecord("source", "stdout", "not-committed", 22)); err != nil {
		t.Fatal(err)
	}
	c.close() // simulate process exit between append and checkpoint
	resumed, err := openLogCapture(tailer)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.close()
	if resumed.cursor("source").Offset != 9 {
		t.Fatal("uncommitted source cursor survived restart")
	}
	got, err := os.ReadFile(a.logPath(tailer.key, "stdout"))
	if err != nil || string(got) != "committed" {
		t.Fatalf("recovered spool = %q, %v", got, err)
	}
}

func TestLogReplayBackpressureIsCancellableWithoutDroppingBytes(t *testing.T) {
	a := New(config.Agent{}, "test", "test", nil, nil)
	a.sendQ = make(chan *agentv1.AgentMessage, 1)
	a.sendQ <- &agentv1.AgentMessage{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.sendLog(ctx, &agentv1.LogChunk{Data: []byte("fresh")}) }()
	select {
	case err := <-done:
		t.Fatalf("full queue silently discarded a log: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	<-a.sendQ
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("draining the queue did not resume output")
	}
	if got := <-a.sendQ; string(got.GetLogChunk().Data) != "fresh" {
		t.Fatalf("queued log = %+v", got)
	}
	a.sendQ <- &agentv1.AgentMessage{}
	go func() { done <- a.sendLog(ctx, &agentv1.LogChunk{}) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancelled replay = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle/full transport blocked capture cancellation")
	}
}

func TestLegacyCapturePreservesExistingMirror(t *testing.T) {
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", nil, nil)
	tailer := &tailer{a: a, key: tailKey{runID: "run-1234", deploymentID: "deployment-1234"}}
	path := a.logPath(tailer.key, "stdout")
	// Create the directory using the regular initializer, then remove only its
	// new checkpoint to model a pre-source-cursor installation.
	c, err := openLogCapture(tailer)
	if err != nil {
		t.Fatal(err)
	}
	c.close()
	if err := os.Remove(a.logCheckpointPath(tailer.key)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("legacy\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c, err = openLogCapture(tailer)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if c.state.Offsets["stdout"] != 7 || len(c.state.Sources) != 0 {
		t.Fatalf("legacy initialization = %+v", c.state)
	}
	if err := c.emit(captureRecord("source", "stdout", "legacy\n", 7)); err != nil {
		t.Fatal(err)
	}
	if err := c.flush(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "legacy\nlegacy\n" {
		t.Fatalf("legacy boundary = %q, %v", data, err)
	}
}

func TestLogRequestReplaysOnlyCommittedBytesAndPersistentEOF(t *testing.T) {
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234"}
	rt := &ownershipRuntime{info: &runtime.ContainerInfo{
		ID: "container", State: "exited",
		Labels: runtime.ManagedLabels(key.deploymentID, key.runID, "recipe", "1", 0, "serving"),
	}}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	c, err := openLogCapture(&tailer{a: a, key: key})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.emit(captureRecord("source", "stdout", "committed", 9)); err != nil {
		t.Fatal(err)
	}
	if err := c.flush(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	<-a.sendQ
	c.close()
	file, err := os.OpenFile(a.logPath(key, "stdout"), os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("uncommitted")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	a.handleLogRequest(t.Context(), &agentv1.LogRequest{
		RunId: key.runID, DeploymentId: key.deploymentID, Stream: "stdout",
	})
	awaitLogChunk(t, a, "stdout", "committed", 0)
	select {
	case message := <-a.sendQ:
		chunk := message.GetLogChunk()
		if chunk == nil || !chunk.Final || chunk.EndOffset != 9 {
			t.Fatalf("persistent final cursor = %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("completed source lost its EOF on replay")
	}
}

type cancelledCompletionRuntime struct {
	runtime.Runtime
	cancel context.CancelFunc
}

func (r *cancelledCompletionRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) runtime.LogCursor, emit func(runtime.LogRecord) error) error {
	if err := emit(captureRecord("completed-source", "stdout", "last bytes", 10)); err != nil {
		return err
	}
	r.cancel()
	return nil // Source EOF is known even though the controller disconnected.
}

func TestCapturePersistsSuccessfulEOFDuringSessionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt := &cancelledCompletionRuntime{cancel: cancel}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234"}
	tailer := &tailer{a: a, key: key, id: "container"}
	complete, err := tailer.capture(ctx)
	if err != nil || !complete {
		t.Fatalf("known EOF was erased by controller cancellation: complete=%v err=%v", complete, err)
	}
	data, err := os.ReadFile(a.logCheckpointPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint logCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if !checkpoint.Completed || checkpoint.Offsets["stdout"] != 10 {
		t.Fatalf("cancelled completion checkpoint = %+v", checkpoint)
	}
	a.rt = &ownershipRuntime{info: &runtime.ContainerInfo{
		ID: "container", State: "exited",
		Labels: runtime.ManagedLabels(key.deploymentID, key.runID, "recipe", "1", 0, "serving"),
	}}
	// Model a new transport; anything queued on the cancelled one is lost.
	a.sendQ = make(chan *agentv1.AgentMessage, 4)
	a.handleLogRequest(t.Context(), &agentv1.LogRequest{
		RunId: key.runID, DeploymentId: key.deploymentID, Stream: "stdout",
	})
	awaitLogChunk(t, a, "stdout", "last bytes", 0)
	select {
	case message := <-a.sendQ:
		if chunk := message.GetLogChunk(); chunk == nil || !chunk.Final || chunk.EndOffset != 10 {
			t.Fatalf("recovered EOF after cancelled transport = %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled transport erased replayable EOF")
	}
}
