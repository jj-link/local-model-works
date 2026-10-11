package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// Docker's timestamp is attached to each engine log record, not to an arbitrary
// read buffer. StdCopy preserves those frame boundaries, including partial and
// very long lines, so no newline scanner or synthesized newline is involved.
type dockerLogCapture struct {
	source   string
	stream   string
	resume   LogCursor
	seen     LogCursor
	emit     func(LogRecord) error
	boundary time.Time
}

func (w *dockerLogCapture) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	stamp, data, ok := bytes.Cut(p, []byte{' '})
	if !ok {
		return 0, fmt.Errorf("logs.timestamp_missing: %s", w.source)
	}
	text := string(stamp)
	timestamp, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0, fmt.Errorf("logs.timestamp_invalid: %w", err)
	}
	if w.seen.Timestamp == text {
		w.seen.Count++
	} else {
		w.seen = LogCursor{Timestamp: text, Count: 1}
	}
	if w.resume.Timestamp != "" {
		if w.boundary.IsZero() {
			w.boundary, err = time.Parse(time.RFC3339Nano, w.resume.Timestamp)
			if err != nil {
				return 0, err
			}
		}
		if timestamp.Before(w.boundary) || (timestamp.Equal(w.boundary) && w.seen.Count <= w.resume.Count) {
			return len(p), nil
		}
	}
	if err := w.emit(LogRecord{Source: w.source, Stream: w.stream, Data: data, Cursor: w.seen}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *dockerRuntime) CaptureLogs(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	return r.captureDockerLogs(ctx, id, true, cursor, emit)
}

func (r *dockerRuntime) CaptureLogSnapshot(ctx context.Context, id string, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	return r.captureDockerLogs(ctx, id, false, cursor, emit)
}

func (r *dockerRuntime) captureDockerLogs(ctx context.Context, id string, follow bool, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	for _, name := range []string{"stdout", "stderr"} {
		go func() { done <- r.captureDockerStream(ctx, id, name, follow, cursor, emit) }()
	}
	var result error
	for range 2 {
		if err := <-done; err != nil && result == nil {
			result = err
			cancel()
		}
	}
	return result
}

func (r *dockerRuntime) captureDockerStream(ctx context.Context, id, name string, follow bool, cursor func(string) LogCursor, emit func(LogRecord) error) error {
	key := "docker:" + id + ":" + name
	for ctx.Err() == nil {
		position := cursor(key)
		if !follow && position.SnapshotComplete {
			return nil
		}
		if follow && position.SnapshotComplete {
			position.SnapshotComplete = false
			if err := emit(LogRecord{Source: key, Stream: name, Cursor: position}); err != nil {
				return err
			}
		}
		// Separate requests avoid rereading all stdout whenever stderr has no
		// cursor (or an old one), while preserving each stream's own boundary.
		rc, err := r.cli.ContainerLogs(ctx, id, container.LogsOptions{
			ShowStdout: name == "stdout", ShowStderr: name == "stderr",
			Follow: follow, Timestamps: true, Since: position.Timestamp,
		})
		if err != nil {
			return fmt.Errorf("logs %s: %w", id, err)
		}
		stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
		writer := &dockerLogCapture{source: key, stream: name, resume: position, emit: emit}
		_, err = stdcopy.StdCopy(writer, writer, rc)
		stop()
		_ = rc.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !follow {
			position = cursor(key)
			position.SnapshotComplete = true
			return emit(LogRecord{Source: key, Stream: name, Cursor: position})
		}
		info, err := r.Inspect(ctx, id)
		if err != nil {
			return err
		}
		if info.State != "running" && info.State != "restarting" && info.State != "paused" {
			position = cursor(key)
			position.SnapshotComplete = true
			return emit(LogRecord{Source: key, Stream: name, Cursor: position})
		}
		// A successful HTTP body EOF is not workload EOF. Engines and proxies
		// can end an idle follow; reopen from committed source cursors.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}

// Closing either demultiplexed reader also interrupts a blocked engine read.
// Closing only io.PipeReader cannot wake StdCopy while the container is idle.
type dockerLogReader struct {
	*io.PipeReader
	closeSource func()
}

func (r *dockerLogReader) Close() error {
	r.closeSource()
	return r.PipeReader.Close()
}

func dockerLogPipes(ctx context.Context, source io.ReadCloser, separate bool) (io.ReadCloser, io.ReadCloser) {
	out, outWriter := io.Pipe()
	errReader, errWriter := io.Pipe()
	var once sync.Once
	closeSource := func() {
		once.Do(func() {
			_ = source.Close()
			_ = outWriter.Close()
			_ = errWriter.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeSource)
	go func() {
		defer stop()
		defer closeSource()
		var err error
		if separate {
			_, err = stdcopy.StdCopy(outWriter, errWriter, source)
		} else {
			_, err = stdcopy.StdCopy(outWriter, outWriter, source)
		}
		_ = outWriter.CloseWithError(err)
		_ = errWriter.CloseWithError(err)
	}()
	return &dockerLogReader{PipeReader: out, closeSource: closeSource}, &dockerLogReader{PipeReader: errReader, closeSource: closeSource}
}
