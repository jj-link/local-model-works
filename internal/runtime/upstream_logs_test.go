//go:build linux

package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type upstreamCaptureTestRuntime struct {
	*upstreamTestRuntime
	opened chan string
}

func (r *upstreamCaptureTestRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	key := "docker:" + id + ":stdout"
	if cursor(key).Offset == 0 {
		if err := emit(LogRecord{Source: key, Stream: "stdout", Data: []byte(id + "\n"), Cursor: LogCursor{Offset: int64(len(id) + 1)}}); err != nil {
			return err
		}
	}
	select {
	case r.opened <- id:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *upstreamCaptureTestRuntime) CaptureLogSnapshot(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	key := "docker:" + id + ":stdout"
	if cursor(key).Offset == 0 {
		return emit(LogRecord{Source: key, Stream: "stdout", Data: []byte(id + "\n"), Cursor: LogCursor{Offset: int64(len(id) + 1)}})
	}
	return nil
}

func TestUpstreamCaptureRetainsIndependentFileAndOwnedContainerCursors(t *testing.T) {
	base := &upstreamCaptureTestRuntime{
		upstreamTestRuntime: &upstreamTestRuntime{containers: map[string]ContainerInfo{
			"authored": {ID: "engine-original", Name: "authored", State: "running"},
		}},
		opened: make(chan string, 4),
	}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	id, err := rt.Create(t.Context(), upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.records[id].Observed["authored"] = "engine-original"
	rt.mu.Unlock()
	if err := os.WriteFile(filepath.Join(rt.root, id, "stdout.log"), []byte("supervisor\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	cursors := make(map[string]LogCursor)
	var output bytes.Buffer
	cursor := func(source string) LogCursor {
		mu.Lock()
		defer mu.Unlock()
		return cursors[source]
	}
	emit := func(record LogRecord) error {
		mu.Lock()
		defer mu.Unlock()
		output.Write(record.Data)
		cursors[record.Source] = record.Cursor
		return nil
	}
	for generation := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- rt.CaptureLogs(ctx, id, cursor, emit) }()
		select {
		case <-base.opened:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("owned Docker source was not attached")
		}
		if generation == 1 {
			base.mu.Lock()
			base.containers["authored"] = ContainerInfo{ID: "engine-replacement", Name: "authored", State: "running"}
			base.mu.Unlock()
			rt.mu.Lock()
			rt.records[id].Observed["authored"] = "engine-replacement"
			rt.mu.Unlock()
			select {
			case opened := <-base.opened:
				if opened != "engine-replacement" {
					t.Fatalf("reattached wrong source: %s", opened)
				}
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("replacement owned source did not attach")
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle upstream sources blocked cancellation")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if output.String() != "supervisor\nengine-original\nengine-replacement\n" {
		t.Fatalf("source restart/replacement changed retained bytes: %q", output.String())
	}
}

func TestUpstreamCaptureWaitsForLiveSupervisorAfterTerminalState(t *testing.T) {
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	id, err := rt.Create(t.Context(), upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	rec := rt.records[id]
	upstreamTestCompletedJob(t, rt, rec, "start")
	identity, err := upstreamProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// A completed lifecycle command can leave its supervised group writing.
	// jobAlive, not the status phase alone, describes that remaining producer.
	rec.Job.PID, rec.Job.ProcessIdentity = os.Getpid(), identity
	path := filepath.Join(rt.root, id, "stdout.log")
	if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	records := make(chan string, 4)
	cursors := make(map[string]LogCursor)
	go func() {
		done <- rt.CaptureLogs(ctx, id, func(source string) LogCursor { return cursors[source] }, func(record LogRecord) error {
			cursors[record.Source] = record.Cursor
			if len(record.Data) > 0 {
				records <- string(record.Data)
			}
			return nil
		})
	}()
	select {
	case data := <-records:
		if data != "initial" {
			t.Fatalf("initial supervisor output = %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("missing initial supervisor output")
	}
	select {
	case err := <-done:
		t.Fatalf("terminal status finalized while producer remained alive: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("late lifecycle output")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-records:
		if data != "late lifecycle output" {
			t.Fatalf("late supervisor output = %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal state lost late lifecycle bytes")
	}
	rt.mu.Lock()
	rec.Job.PID = 0 // The durable producer is now confirmed gone.
	rt.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture did not finalize after producer exit")
	}
}

type retiringCaptureRuntime struct {
	*upstreamTestRuntime
	release chan struct{}
	opened  chan string
}

func (r *retiringCaptureRuntime) emitRetained(id, text string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	source := "docker:" + id + ":stdout"
	position := cursor(source)
	if position.Offset >= int64(len(text)) {
		return nil
	}
	return emit(LogRecord{Source: source, Stream: "stdout", Data: []byte(text[position.Offset:]), Cursor: LogCursor{Offset: int64(len(text))}})
}

func (r *retiringCaptureRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	text := "replacement"
	if id == "old-engine" {
		text = "old-prefix"
	}
	if err := r.emitRetained(id, text, cursor, emit); err != nil {
		return err
	}
	r.opened <- id
	if id == "old-engine" {
		select {
		case <-r.release:
			return r.emitRetained(id, "old-prefix-retained-tail", cursor, emit)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *retiringCaptureRuntime) CaptureLogSnapshot(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	if id == "old-engine" {
		select {
		case <-r.release:
			return r.emitRetained(id, "old-prefix-retained-tail", cursor, emit)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.emitRetained(id, "replacement", cursor, emit)
}

func TestUpstreamCaptureResumesPendingRetiredContainerAfterRestart(t *testing.T) {
	base := &retiringCaptureRuntime{
		upstreamTestRuntime: &upstreamTestRuntime{containers: map[string]ContainerInfo{
			"authored": {ID: "old-engine", Name: "authored", State: "running"},
		}},
		release: make(chan struct{}), opened: make(chan string, 8),
	}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	id, err := rt.Create(t.Context(), upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	rec := rt.records[id]
	rec.Observed["authored"] = "old-engine"
	if err := rt.save(rec); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	cursors := make(map[string]LogCursor)
	data := make(chan string, 8)
	get := func(source string) LogCursor {
		mu.Lock()
		defer mu.Unlock()
		return cursors[source]
	}
	emit := func(record LogRecord) error {
		mu.Lock()
		cursors[record.Source] = record.Cursor
		mu.Unlock()
		if len(record.Data) > 0 {
			data <- string(record.Data)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rt.CaptureLogs(ctx, id, get, emit) }()
	select {
	case <-base.opened:
	case <-time.After(time.Second):
		t.Fatal("old source did not attach")
	}
	if got := <-data; got != "old-prefix" {
		t.Fatalf("old prefix = %q", got)
	}
	base.mu.Lock()
	base.containers["retained-original"] = ContainerInfo{ID: "old-engine", Name: "retained-original", State: "exited"}
	base.containers["authored"] = ContainerInfo{ID: "new-engine", Name: "authored", State: "running"}
	base.mu.Unlock()
	rt.mu.Lock()
	rec.Observed["authored"] = "new-engine"
	err = rt.save(rec)
	rt.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case opened := <-base.opened:
		if opened != "new-engine" {
			t.Fatalf("replacement attachment = %q", opened)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement source did not attach")
	}
	if got := <-data; got != "replacement" {
		t.Fatalf("replacement data = %q", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pending retired snapshot blocked cancellation")
	}
	// Reload actual durable runtime identity, retaining only agent source cursors.
	rt = upstreamTestWrapper(t, base, root)
	close(base.release)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	go func() { done <- rt.CaptureLogs(ctx, id, get, emit) }()
	select {
	case got := <-data:
		if got != "-retained-tail" {
			t.Fatalf("retired source replayed or lost its prefix: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("restart discarded retained backlog from the replaced immutable ID")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resumed retired source did not close")
	}
}

func TestUpstreamReloadPreservesPreReplacementLogIdentity(t *testing.T) {
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	id, err := rt.Create(t.Context(), upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	rec := rt.records[id]
	rec.Observed["authored"] = "old-engine"
	upstreamTestCompletedJob(t, rt, rec, "start")
	rec.LogSources = nil // Model a legacy record written before source history.
	if err := writeUpstreamJSON(filepath.Join(rt.root, id, "record.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := writeUpstreamJSON(filepath.Join(rec.Job.Dir, "status.json"), &upstreamStatus{
		Done: true, Phase: "started", Observed: map[string]string{"authored": "new-engine"},
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := upstreamTestWrapper(t, base, root)
	got := reloaded.records[id]
	if got.Observed["authored"] != "new-engine" || got.LogSources["old-engine"] != "authored" || got.LogSources["new-engine"] != "authored" {
		t.Fatalf("status import lost immutable source history: %+v", got)
	}
	again := upstreamTestWrapper(t, base, root)
	if again.records[id].LogSources["old-engine"] != "authored" {
		t.Fatal("historical source identity was not durably migrated")
	}
}
