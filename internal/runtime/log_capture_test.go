package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func TestDockerCaptureResumesTimestampBoundaryWithoutChangingBytes(t *testing.T) {
	const stamp = "2026-10-11T18:00:00.123456789Z"
	cursor := LogCursor{Timestamp: stamp, Count: 1}
	var output bytes.Buffer
	var committed LogCursor
	writer := &dockerLogCapture{source: "docker:one:stdout", stream: "stdout", resume: cursor, emit: func(record LogRecord) error {
		output.Write(record.Data)
		committed = record.Cursor
		return nil
	}}
	partial := strings.Repeat("x", 2*1024*1024) + "\rpartial"
	for _, data := range []string{"already committed\n", partial, "same timestamp, distinct record\n"} {
		if _, err := writer.Write([]byte(stamp + " " + data)); err != nil {
			t.Fatal(err)
		}
	}
	if output.String() != partial+"same timestamp, distinct record\n" || committed.Count != 3 {
		t.Fatalf("timestamp-boundary output changed: size %d, cursor %+v", output.Len(), committed)
	}
	writer = &dockerLogCapture{source: "docker:one:stdout", stream: "stdout", resume: committed, emit: func(record LogRecord) error {
		t.Fatalf("restart replayed committed record: %q", record.Data)
		return nil
	}}
	for _, data := range []string{"already committed\n", partial, "same timestamp, distinct record\n"} {
		if _, err := writer.Write([]byte(stamp + " " + data)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDockerCaptureReopensSpontaneousEOFWhileContainerRunning(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("timestamps") != "1" && r.URL.Query().Get("timestamps") != "true" {
				t.Errorf("capture did not request source timestamps: %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			if r.URL.Query().Get("stdout") != "1" && r.URL.Query().Get("stdout") != "true" {
				return
			}
			request := requests.Add(1)
			stdout := stdcopy.NewStdWriter(w, stdcopy.Stdout)
			_, _ = io.WriteString(stdout, "2026-10-11T18:00:00.000000001Z before\n")
			if request > 1 {
				_, _ = io.WriteString(stdout, "2026-10-11T18:00:01.000000001Z fresh-partial")
			}
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"container","Name":"/container","State":{"Status":"running"},"Config":{"Labels":{}},"NetworkSettings":{"Ports":{}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	rt := &dockerRuntime{cli: cli}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cursors := make(map[string]LogCursor)
	var output bytes.Buffer
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- rt.CaptureLogs(ctx, "container", func(source string) LogCursor {
			mu.Lock()
			defer mu.Unlock()
			return cursors[source]
		}, func(record LogRecord) error {
			mu.Lock()
			defer mu.Unlock()
			output.Write(record.Data)
			cursors[record.Source] = record.Cursor
			if strings.Contains(output.String(), "fresh-partial") {
				cancel()
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("running container stayed detached after stream EOF")
	}
	if output.String() != "before\nfresh-partial" || requests.Load() < 2 {
		t.Fatalf("reattached output = %q, requests = %d", output.String(), requests.Load())
	}
}

type closeObservedReader struct {
	*io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func (r *closeObservedReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return r.PipeReader.Close()
}

func TestDockerReaderCloseInterruptsIdleEngineRead(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			source := &closeObservedReader{PipeReader: reader, closed: make(chan struct{})}
			stdout, stderr := dockerLogPipes(t.Context(), source, separate)
			defer stderr.Close()
			if err := stdout.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-source.closed:
			case <-time.After(time.Second):
				t.Fatal("closing idle output left engine HTTP body open")
			}
		})
	}
}

func TestCaptureFilesResumeIndependentlyAcrossRotationAndRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stdout.log")
	otherPath := filepath.Join(root, "engine.log")
	for path, text := range map[string]string{path: "supervisor\rpartial", otherPath: "engine\n"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cursors := make(map[string]LogCursor)
	var output bytes.Buffer
	get := func(source string) LogCursor { return cursors[source] }
	emit := func(record LogRecord) error {
		output.Write(record.Data)
		cursors[record.Source] = record.Cursor
		return nil
	}
	drain := func(file *captureFile) {
		t.Helper()
		if err := file.drain(t.Context(), root, get, emit); err != nil {
			t.Fatal(err)
		}
	}
	first, other := &captureFile{path: path, stream: "stdout"}, &captureFile{path: otherPath, stream: "stdout"}
	drain(first)
	drain(other)
	first.close()
	other.close()
	first, other = &captureFile{path: path, stream: "stdout"}, &captureFile{path: otherPath, stream: "stdout"}
	defer first.close()
	defer other.close()
	drain(other) // changed source scheduling must not replay or skip bytes
	drain(first)
	if output.String() != "supervisor\rpartialengine\n" {
		t.Fatalf("restart replayed source history: %q", output.String())
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	old, err := os.OpenFile(path+".1", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = old.WriteString("old-tail")
	_ = old.Close()
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	drain(first)
	if !strings.HasSuffix(output.String(), "old-tailreplacement") {
		t.Fatalf("rotation lost an old or replacement tail: %q", output.String())
	}
	// Same inode, truncate-and-regrow beyond the previous offset.
	if err := os.WriteFile(path, []byte("new-generation-longer-than-before"), 0600); err != nil {
		t.Fatal(err)
	}
	drain(first)
	if !strings.HasSuffix(output.String(), "replacementnew-generation-longer-than-before") {
		t.Fatalf("copytruncate skipped valid replacement bytes: %q", output.String())
	}
}

func TestFileCaptureDoesNotCommitMixedGenerationAnchor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stdout.log")
	const chunkSize = 256 * 1024
	old := bytes.Repeat([]byte("a"), chunkSize+512)
	replacement := bytes.Repeat([]byte("b"), len(old))
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	file := &captureFile{path: path, stream: "stdout"}
	defer file.close()
	var position LogCursor
	var output bytes.Buffer
	replaced := false
	get := func(string) LogCursor { return position }
	emit := func(record LogRecord) error {
		output.Write(record.Data)
		position = record.Cursor
		if !replaced && len(record.Data) > 0 {
			replaced = true
			// Copytruncate/regrow between two reads, without changing inode.
			return os.WriteFile(path, replacement, 0600)
		}
		return nil
	}
	for range 2 {
		if err := file.drain(t.Context(), root, get, emit); err != nil {
			t.Fatal(err)
		}
	}
	want := append(append([]byte(nil), old[:chunkSize]...), replacement...)
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("replacement prefix was lost behind an uncaptured anchor: received %d bytes, want %d", output.Len(), len(want))
	}
}

type rewritingLogReader struct {
	file    *os.File
	path    string
	replace []byte
}

func (r *rewritingLogReader) ReadAt(data []byte, offset int64) (int, error) {
	n, err := r.file.ReadAt(data, offset)
	if r.replace != nil {
		replacement := r.replace
		r.replace = nil
		if writeErr := os.WriteFile(r.path, replacement, 0600); writeErr != nil {
			return n, writeErr
		}
	}
	return n, err
}

func TestFileCaptureAnchorUsesPayloadAcrossImmediateRewrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "stdout.log")
	old, replacement := bytes.Repeat([]byte("A"), 300), bytes.Repeat([]byte("B"), 400)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	position := LogCursor{FileID: logFileIdentity(stat)}
	data := make([]byte, len(old))
	// The rewrite happens after ReadAt fills data but before it returns to the
	// capture helper: no timing-dependent goroutine race or production hook.
	n, anchor, err := readLogFileChunk(&rewritingLogReader{file: file, path: path, replace: replacement}, position, data)
	if err != nil {
		t.Fatal(err)
	}
	position.Offset, position.Anchor = int64(n), anchor
	var output bytes.Buffer
	output.Write(data[:n])
	source := &captureFile{file: file, path: path, stream: "stdout"}
	if err := source.drain(t.Context(), root, func(string) LogCursor { return position }, func(record LogRecord) error {
		position = record.Cursor
		output.Write(record.Data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), old...), replacement...); !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("anchor certified uncaptured replacement bytes: output length=%d want=%d", output.Len(), len(want))
	}
}

func TestDockerRetiredSnapshotIsBoundedAndCheckpointed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			t.Errorf("retired snapshot requested live lifecycle state: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if follow := r.URL.Query().Get("follow"); follow == "1" || follow == "true" {
			t.Error("retired source still follows new output")
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		if stdout := r.URL.Query().Get("stdout"); stdout == "1" || stdout == "true" {
			_, _ = io.WriteString(stdcopy.NewStdWriter(w, stdcopy.Stdout), "2026-10-11T18:00:00.000000001Z retained-tail")
		}
	}))
	defer server.Close()
	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.47"), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	rt := &dockerRuntime{cli: cli}
	var mu sync.Mutex
	cursors := make(map[string]LogCursor)
	var output bytes.Buffer
	get := func(source string) LogCursor {
		mu.Lock()
		defer mu.Unlock()
		return cursors[source]
	}
	emit := func(record LogRecord) error {
		mu.Lock()
		defer mu.Unlock()
		cursors[record.Source] = record.Cursor
		output.Write(record.Data)
		return nil
	}
	if err := rt.CaptureLogSnapshot(t.Context(), "retired", get, emit); err != nil {
		t.Fatal(err)
	}
	if output.String() != "retained-tail" || !cursors["docker:retired:stdout"].SnapshotComplete || !cursors["docker:retired:stderr"].SnapshotComplete {
		t.Fatalf("retired snapshot output=%q cursors=%+v", output.String(), cursors)
	}
	before := requests.Load()
	if err := rt.CaptureLogSnapshot(t.Context(), "retired", get, emit); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before || output.String() != "retained-tail" {
		t.Fatal("completed retired snapshot was opened or appended again")
	}
}
