package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/runtime"
)

type logAttachment struct {
	container string
	stdout    *io.PipeWriter
	stderr    *io.PipeWriter
}

type reconnectLogRuntime struct {
	runtime.Runtime
	opened chan logAttachment
}

func (r *reconnectLogRuntime) LogsStreams(_ context.Context, id string) (io.ReadCloser, io.ReadCloser, error) {
	stdout, out := io.Pipe()
	stderr, errOut := io.Pipe()
	r.opened <- logAttachment{container: id, stdout: out, stderr: errOut}
	return stdout, stderr, nil
}

func (r *reconnectLogRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) runtime.LogCursor, emit func(runtime.LogRecord) error) error {
	stdout, stderr, err := r.LogsStreams(ctx, id)
	if err != nil {
		return err
	}
	closeReaders := func() { _ = stdout.Close(); _ = stderr.Close() }
	stop := context.AfterFunc(ctx, closeReaders)
	defer stop()
	defer closeReaders()
	results := make(chan error, 2)
	for name, reader := range map[string]io.ReadCloser{"stdout": stdout, "stderr": stderr} {
		go func() {
			source := id + ":" + name
			position := cursor(source)
			buf := make([]byte, 256*1024)
			for {
				n, err := reader.Read(buf)
				if n > 0 {
					position.Offset += int64(n)
					if emitErr := emit(runtime.LogRecord{Source: source, Stream: name, Data: buf[:n], Cursor: position}); emitErr != nil {
						results <- emitErr
						return
					}
				}
				if err != nil {
					if err == io.EOF {
						err = nil
					}
					results <- err
					return
				}
			}
		}()
	}
	first, second := <-results, <-results
	if first != nil {
		return first
	}
	return second
}

func (r *reconnectLogRuntime) Inspect(context.Context, string) (*runtime.ContainerInfo, error) {
	return &runtime.ContainerInfo{State: "running"}, nil
}

func cleanupLogTailers(t *testing.T, a *Agent) {
	t.Helper()
	t.Cleanup(func() {
		// Context cancellation alone does not wait for the final checkpoint.
		// Join it before TempDir cleanup and before the next test starts doing
		// its own durable I/O on the same filesystem.
		a.workloads.mu.Lock()
		tailers := make([]*tailer, 0, len(a.workloads.tailers))
		for _, active := range a.workloads.tailers {
			active.cancel()
			tailers = append(tailers, active)
		}
		a.workloads.mu.Unlock()
		for _, active := range tailers {
			<-active.done
		}
	})
}

func awaitLogAttachment(t *testing.T, a *Agent, r *reconnectLogRuntime, key tailKey) logAttachment {
	t.Helper()
	select {
	case attachment := <-r.opened:
		t.Cleanup(func() {
			attachment.stdout.Close()
			attachment.stderr.Close()
		})
		return attachment
	case <-time.After(2 * time.Second):
		failLogCaptureWait(t, a, key, "running workload did not acquire a log reader")
		return logAttachment{}
	}
}

func failLogCaptureWait(t *testing.T, a *Agent, key tailKey, reason string) {
	t.Helper()
	a.workloads.mu.Lock()
	active := a.workloads.tailers[key]
	a.workloads.mu.Unlock()
	state := "absent"
	if active != nil {
		finished := false
		select {
		case <-active.done:
			finished = true
		default:
		}
		state = fmt.Sprintf("container=%s context=%v done=%v", active.id, active.ctx.Err(), finished)
	}
	// Initialization errors otherwise sit unread in sendQ while the test waits.
	// Summarize chunk metadata instead of dumping potentially megabytes of data.
	var queued []string
	for range len(a.sendQ) {
		message := <-a.sendQ
		if chunk := message.GetLogChunk(); chunk != nil {
			queued = append(queued, fmt.Sprintf("LogChunk{stream=%q offset=%d bytes=%d final=%v end=%d}",
				chunk.Stream, chunk.Offset, len(chunk.Data), chunk.Final, chunk.EndOffset))
		} else {
			queued = append(queued, message.String())
		}
	}
	stack := make([]byte, 1024*1024)
	n := goruntime.Stack(stack, true)
	t.Fatalf("%s: tailer={%s}; queued=%v\n%s", reason, state, queued, stack[:n])
}

func awaitLogChunk(t *testing.T, a *Agent, stream, text string, offset uint64) {
	t.Helper()
	select {
	case message := <-a.sendQ:
		chunk := message.GetLogChunk()
		if chunk == nil || chunk.GetStream() != stream || string(chunk.GetData()) != text || chunk.GetOffset() != offset || chunk.GetFinal() {
			t.Fatalf("log event = %+v; want %s data %q at byte %d", message, stream, text, offset)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("missing %s log data %q after tailer transition", stream, text)
	}
}

func TestLogTailerResumesSameContainerAfterSessionCancellation(t *testing.T) {
	rt := &reconnectLogRuntime{opened: make(chan logAttachment, 4)}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	cleanupLogTailers(t, a)
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234", rank: 0}
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	t.Cleanup(cancelFirst)
	a.workloads.startTailer(firstCtx, key.runID, key.deploymentID, key.rank, "same-container")
	first := awaitLogAttachment(t, a, rt, key)
	if _, err := io.WriteString(first.stdout, "before reconnect\n"); err != nil {
		t.Fatal(err)
	}
	awaitLogChunk(t, a, "stdout", "before reconnect\n", 0)

	// Reconcile can arrive before the previous reader goroutines have exited.
	cancelFirst()
	a.workloads.startTailer(t.Context(), key.runID, key.deploymentID, key.rank, "same-container")
	second := awaitLogAttachment(t, a, rt, key)
	if _, err := io.WriteString(second.stdout, "after reconnect\n"); err != nil {
		t.Fatal(err)
	}
	awaitLogChunk(t, a, "stdout", "after reconnect\n", uint64(len("before reconnect\n")))
	if _, err := io.WriteString(second.stderr, "resumed diagnostic\n"); err != nil {
		t.Fatal(err)
	}
	awaitLogChunk(t, a, "stderr", "resumed diagnostic\n", 0)
	contents, err := os.ReadFile(a.logPath(key, "stdout"))
	if err != nil || string(contents) != "before reconnect\nafter reconnect\n" {
		t.Fatalf("retained log = %q, %v", contents, err)
	}
}

func TestReplacingLogTailerClosesOldReaderWithoutStoppingReplacement(t *testing.T) {
	rt := &reconnectLogRuntime{opened: make(chan logAttachment, 4)}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	cleanupLogTailers(t, a)
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234", rank: 0}
	a.workloads.startTailer(t.Context(), key.runID, key.deploymentID, key.rank, "old-container")
	first := awaitLogAttachment(t, a, rt, key)
	a.workloads.startTailer(t.Context(), key.runID, key.deploymentID, key.rank, "replacement-container")
	second := awaitLogAttachment(t, a, rt, key)
	if second.container != "replacement-container" {
		t.Fatalf("attached to %q instead of replacement", second.container)
	}
	oldWrite := make(chan error, 1)
	go func() {
		_, err := io.WriteString(first.stdout, "obsolete output\n")
		oldWrite <- err
	}()
	select {
	case err := <-oldWrite:
		if err != io.ErrClosedPipe {
			t.Fatalf("old reader remains writable after replacement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old reader remained blocked after replacement")
	}
	if _, err := io.WriteString(second.stdout, "replacement output\n"); err != nil {
		t.Fatal(err)
	}
	awaitLogChunk(t, a, "stdout", "replacement output\n", 0)
	a.workloads.stopTailer(key.runID, key.deploymentID, key.rank)
	a.workloads.stopTailer(key.runID, key.deploymentID, key.rank)
}

func TestLogTailerReattachesAfterCleanStreamEnd(t *testing.T) {
	rt := &reconnectLogRuntime{opened: make(chan logAttachment, 4)}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", rt, nil)
	cleanupLogTailers(t, a)
	key := tailKey{runID: "run-1234", deploymentID: "deployment-1234", rank: 0}
	a.workloads.startTailer(t.Context(), key.runID, key.deploymentID, key.rank, "same-container")
	first := awaitLogAttachment(t, a, rt, key)
	a.workloads.mu.Lock()
	firstGeneration := a.workloads.tailers[key]
	ended := firstGeneration.done
	a.workloads.mu.Unlock()
	first.stdout.Close()
	first.stderr.Close()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("ended stream retained its tailer")
	}
	a.workloads.mu.Lock()
	retained := a.workloads.tailers[key]
	a.workloads.mu.Unlock()
	if retained != nil {
		t.Fatalf("completed generation remains registered: old=%p current=%p", firstGeneration, retained)
	}
	finals := map[string]bool{}
	for range 2 {
		select {
		case message := <-a.sendQ:
			chunk := message.GetLogChunk()
			if chunk == nil || !chunk.GetFinal() || chunk.GetEndOffset() != 0 {
				t.Fatalf("clean stream end = %+v", message)
			}
			finals[chunk.GetStream()] = true
		case <-time.After(2 * time.Second):
			t.Fatal("clean stream end lost its final byte cursor")
		}
	}
	if !finals["stdout"] || !finals["stderr"] {
		t.Fatalf("final stream cursors = %v", finals)
	}
	a.workloads.startTailer(t.Context(), key.runID, key.deploymentID, key.rank, "same-container")
	a.workloads.mu.Lock()
	replacement := a.workloads.tailers[key]
	a.workloads.mu.Unlock()
	if replacement == nil || replacement == firstGeneration {
		t.Fatalf("reattach failed to register a fresh generation: old=%p current=%p", firstGeneration, replacement)
	}
	if err := replacement.ctx.Err(); err != nil {
		t.Fatalf("new generation inherited a cancelled context: %v", err)
	}
	second := awaitLogAttachment(t, a, rt, key)
	if _, err := io.WriteString(second.stdout, "reattached output\n"); err != nil {
		t.Fatal(err)
	}
	awaitLogChunk(t, a, "stdout", "reattached output\n", 0)
}
