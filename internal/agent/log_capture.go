package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// A checkpoint commits both local byte lengths and source cursors. Appends after
// that boundary are uncommitted: discard them on restart before re-reading the
// source. Only committed bytes are sent, including through history requests.
type logCheckpoint struct {
	Offsets   map[string]int64             `json:"offsets"`
	Sources   map[string]runtime.LogCursor `json:"sources"`
	Completed bool                         `json:"completed,omitempty"`
}

type logCapture struct {
	mu        sync.Mutex
	t         *tailer
	files     map[string]*os.File
	state     logCheckpoint
	committed map[string]int64
	dirty     bool
	err       error
}

func (a *Agent) logCheckpointPath(key tailKey) string {
	return strings.TrimSuffix(a.logPath(key, "stdout"), "-stdout.log") + "-capture.json"
}

func openLogCapture(t *tailer) (*logCapture, error) {
	c := &logCapture{t: t, files: make(map[string]*os.File), committed: make(map[string]int64)}
	path := t.a.logCheckpointPath(t.key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	legacy := errors.Is(err, os.ErrNotExist)
	if err != nil && !legacy {
		return nil, err
	}
	if !legacy {
		if err := json.Unmarshal(data, &c.state); err != nil {
			return nil, err
		}
		if c.state.Offsets == nil || c.state.Sources == nil {
			return nil, fmt.Errorf("logs.checkpoint_invalid")
		}
	} else {
		// Old merged mirrors contain no source identities. Keep every byte;
		// one initial source-history replay establishes unambiguous cursors.
		c.state.Offsets = make(map[string]int64)
		c.state.Sources = make(map[string]runtime.LogCursor)
	}
	for _, name := range []string{"stdout", "stderr"} {
		f, err := os.OpenFile(t.a.logPath(t.key, name), os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			c.close()
			return nil, err
		}
		c.files[name] = f
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			c.close()
			return nil, err
		}
		if legacy {
			c.state.Offsets[name] = end
		}
		offset := c.state.Offsets[name]
		if offset < 0 || offset > end {
			c.close()
			return nil, fmt.Errorf("logs.checkpoint_out_of_range: %s", name)
		}
		if err := f.Truncate(offset); err != nil {
			c.close()
			return nil, err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			c.close()
			return nil, err
		}
		c.committed[name] = offset
	}
	c.state.Completed = false
	c.dirty = true
	if err := c.flush(context.Background(), false); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *logCapture) close() {
	for _, file := range c.files {
		_ = file.Close()
	}
}

func (c *logCapture) cursor(source string) runtime.LogCursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Sources[source]
}

func (c *logCapture) emit(record runtime.LogRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	file := c.files[record.Stream]
	if file == nil || record.Source == "" {
		return fmt.Errorf("logs.source_invalid")
	}
	n, err := file.Write(record.Data)
	if err == nil && n != len(record.Data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.err = err
		return err
	}
	c.state.Offsets[record.Stream] += int64(n)
	c.state.Sources[record.Source] = record.Cursor
	c.dirty = true
	return nil
}

// Batch checkpoints once per tick instead of fsync/JSON per application line.
// The file bytes are durable before the atomic cursor checkpoint is installed.
func (c *logCapture) flush(ctx context.Context, completed bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if completed {
		c.state.Completed = true
		c.dirty = true
	}
	if !c.dirty {
		return nil
	}
	for _, file := range c.files {
		if err := file.Sync(); err != nil {
			c.err = err
			return err
		}
	}
	if err := saveLogCheckpoint(c.t.a.logCheckpointPath(c.t.key), &c.state); err != nil {
		c.err = err
		return err
	}
	c.dirty = false
	for _, name := range []string{"stdout", "stderr"} {
		from, end := c.committed[name], c.state.Offsets[name]
		c.committed[name] = end
		if from == end || ctx.Err() != nil {
			continue
		}
		if err := c.t.a.replayLogFile(ctx, c.t.key, name, c.files[name], uint64(from), uint64(end)); err != nil {
			return err
		}
	}
	return nil
}

func saveLogCheckpoint(path string, state *logCheckpoint) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".capture-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *Agent) sendLog(ctx context.Context, chunk *agentv1.LogChunk) error {
	select {
	case a.sendQ <- &agentv1.AgentMessage{Body: &agentv1.AgentMessage_LogChunk{LogChunk: chunk}}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) replayLogFile(ctx context.Context, key tailKey, name string, file *os.File, from, end uint64) error {
	for from < end {
		data := make([]byte, min(end-from, 256*1024))
		n, err := file.ReadAt(data, int64(from))
		if n > 0 {
			if sendErr := a.sendLog(ctx, &agentv1.LogChunk{
				RunId: key.runID, DeploymentId: key.deploymentID, Rank: key.rank,
				Stream: name, Offset: from, Data: data[:n],
			}); sendErr != nil {
				return sendErr
			}
			from += uint64(n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
