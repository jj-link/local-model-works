package fakeagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/jj-link/local-model-works/internal/runtime"
)

// FakeContainer is one in-memory container.
type FakeContainer struct {
	ID        string
	Name      string
	Spec      runtime.ContainerSpec
	State     string // created | running | exited
	ExitCode  int
	Error     string
	OOMKilled bool
	logs      map[string]*logBuf
}

// logBuf is a growing byte buffer with blocking readers (agent tailer).
type logBuf struct {
	mu      sync.Mutex
	data    []byte
	closed  bool
	waiters []chan struct{}
}

func (b *logBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	for _, w := range b.waiters {
		close(w)
	}
	b.waiters = nil
	b.mu.Unlock()
	return len(p), nil
}

func (b *logBuf) Close() {
	b.mu.Lock()
	b.closed = true
	for _, w := range b.waiters {
		close(w)
	}
	b.waiters = nil
	b.mu.Unlock()
}

// Size returns the current byte count.
func (b *logBuf) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.data)
}

// ReadFrom returns bytes at/after offset, blocking until data or close.
// ok=false means the stream is closed and fully drained.
func (b *logBuf) ReadFrom(offset int) (out []byte, ok bool) {
	for {
		b.mu.Lock()
		if offset < len(b.data) {
			out = append([]byte(nil), b.data[offset:]...)
			b.mu.Unlock()
			return out, true
		}
		if b.closed {
			b.mu.Unlock()
			return nil, false
		}
		w := make(chan struct{})
		b.waiters = append(b.waiters, w)
		b.mu.Unlock()
		<-w
	}
}

// logReader adapts a logBuf to io.Reader for the tailer's bufio.Scanner.
type logReader struct {
	buf    *logBuf
	offset int
}

func (r *logReader) Read(p []byte) (int, error) {
	chunk, ok := r.buf.ReadFrom(r.offset)
	if !ok && len(chunk) == 0 {
		return 0, io.EOF
	}
	if len(chunk) > len(p) {
		chunk = chunk[:len(p)]
	}
	n := copy(p, chunk)
	r.offset += n
	return n, nil
}

// FakeRuntime is an in-memory runtime.Runtime: containers keyed by name,
// spec-preserving, idempotent stop/remove, tailer-readable log buffers.
type FakeRuntime struct {
	mu               sync.Mutex
	byName           map[string]*FakeContainer
	nextID           int
	pullLog          []string
	hostPreparations int
}

// NewFakeRuntime returns an empty stub runtime.
func NewFakeRuntime() *FakeRuntime {
	return &FakeRuntime{byName: map[string]*FakeContainer{}}
}

// Pulls lists every pulled image reference (digest-pinned by protocol).
func (rt *FakeRuntime) Pulls() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.pullLog...)
}

// Ping reports a Docker-like engine version.
func (rt *FakeRuntime) Ping(context.Context) (string, error) { return "27.0.0-fake", nil }

// Pull records the reference.
func (rt *FakeRuntime) Pull(ctx context.Context, spec *runtime.PullSpec) error {
	rt.mu.Lock()
	rt.pullLog = append(rt.pullLog, spec.Reference)
	rt.mu.Unlock()
	return nil
}

// PrepareHost records one successful bounded host-preparation operation.
func (rt *FakeRuntime) PrepareHost(context.Context, *runtime.ContainerSpec) error {
	rt.mu.Lock()
	rt.hostPreparations++
	rt.mu.Unlock()
	return nil
}

// Create stores the spec; duplicate names conflict like Docker.
func (rt *FakeRuntime) Create(ctx context.Context, spec *runtime.ContainerSpec) (string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.byName[spec.Name]; ok {
		return "", fmt.Errorf("Error response from daemon: Conflict. The container name %q is already in use", spec.Name)
	}
	rt.nextID++
	id := strconv.FormatInt(int64(0xfa1e0000+rt.nextID), 16)
	c := &FakeContainer{
		ID: id, Name: spec.Name, Spec: *spec, State: "created",
		logs: map[string]*logBuf{"stdout": {}, "stderr": {}},
	}
	rt.byName[spec.Name] = c
	return id, nil
}

// Start transitions created→running; starting a running container fails the
// way the engine does (the agent treats it as "already running").
func (rt *FakeRuntime) Start(ctx context.Context, id string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.get(id)
	if c == nil {
		return fmt.Errorf("Error: No such container: %s", id)
	}
	switch c.State {
	case "created":
		if c.Spec.Labels[runtime.LabelModule] == "extension" {
			c.State = "exited"
			_, _ = c.logs["stdout"].Write([]byte(`{"version":1}` + "\n"))
			c.logs["stdout"].Close()
			c.logs["stderr"].Close()
			return nil
		}
		c.State = "running"
		return nil
	case "running":
		return errors.New("Error response from daemon: container is already running")
	default:
		return fmt.Errorf("Error response from daemon: container %s already started (exited)", c.Name)
	}
}

// Stop transitions running→exited (idempotent); closing log streams so the
// tailer sees EOF and sends its final chunk.
func (rt *FakeRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	rt.mu.Lock()
	c := rt.get(id)
	if c == nil {
		rt.mu.Unlock()
		return nil // idempotent
	}
	if c.State == "running" {
		c.State = "exited"
		c.ExitCode = 0
	}
	for _, l := range c.logs {
		l.Close()
	}
	rt.mu.Unlock()
	return nil
}

// Remove deletes the container (force allows running).
func (rt *FakeRuntime) Remove(ctx context.Context, id string, force bool) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.get(id)
	if c == nil {
		return nil // idempotent
	}
	if c.State == "running" && !force {
		return errors.New("Error response from daemon: cannot remove running container")
	}
	delete(rt.byName, c.Name)
	return nil
}

// Inspect returns observed state by id or name.
func (rt *FakeRuntime) Inspect(ctx context.Context, idOrName string) (*runtime.ContainerInfo, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	c := rt.get(idOrName)
	if c == nil {
		return nil, fmt.Errorf("Error: No such container: %s", idOrName)
	}
	return &runtime.ContainerInfo{
		ID: c.ID, Name: c.Name, State: c.State, ExitCode: c.ExitCode, Error: c.Error,
		OOMKilled: c.OOMKilled, Labels: c.Spec.Labels,
	}, nil
}

// ListByLabel returns containers carrying the label pair.
func (rt *FakeRuntime) ListByLabel(ctx context.Context, key, value string) ([]runtime.ContainerInfo, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []runtime.ContainerInfo
	for _, c := range rt.byName {
		if c.Spec.Labels[key] == value {
			out = append(out, runtime.ContainerInfo{
				ID: c.ID, Name: c.Name, State: c.State, ExitCode: c.ExitCode, Error: c.Error,
				OOMKilled: c.OOMKilled, Labels: c.Spec.Labels,
			})
		}
	}
	return out, nil
}

// get looks up by name then id.
func (rt *FakeRuntime) get(idOrName string) *FakeContainer {
	if c, ok := rt.byName[idOrName]; ok {
		return c
	}
	for _, c := range rt.byName {
		if c.ID == idOrName || c.Name == idOrName {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test-side inspection and log writing
// ---------------------------------------------------------------------------

// Containers lists all container names.
func (rt *FakeRuntime) Containers() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, 0, len(rt.byName))
	for n := range rt.byName {
		out = append(out, n)
	}
	return out
}

// Container returns the container named name (may be nil).
func (rt *FakeRuntime) Container(name string) *FakeContainer {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.byName[name]
}

// StateOf returns a container's state ("", if gone).
func (rt *FakeRuntime) StateOf(name string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if c := rt.byName[name]; c != nil {
		return c.State
	}
	return ""
}

// IDOf returns a container's id ("", if gone).
func (rt *FakeRuntime) IDOf(name string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if c := rt.byName[name]; c != nil {
		return c.ID
	}
	return ""
}

// WriteLog appends bytes to a container's stream (drives the tailer).
func (rt *FakeRuntime) WriteLog(name, stream string, data []byte) {
	rt.mu.Lock()
	c := rt.byName[name]
	rt.mu.Unlock()
	if c == nil {
		return
	}
	c.logs[stream].Write(data)
}

// LogSize returns the stream's byte count.
func (rt *FakeRuntime) LogSize(name, stream string) int {
	rt.mu.Lock()
	c := rt.byName[name]
	rt.mu.Unlock()
	if c == nil {
		return 0
	}
	return c.logs[stream].Size()
}

// RemoveExternally deletes a container without the runtime API (simulates an
// external actor, e.g. "the machine died").
func (rt *FakeRuntime) RemoveExternally(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.byName, name)
}

// ExitExternally transitions a running container to a terminal state without
// using Runtime.Stop, simulating a workload crash observed by the monitor.
func (rt *FakeRuntime) ExitExternally(name string, exitCode int, oomKilled bool, errMessage string) {
	rt.mu.Lock()
	container := rt.get(name)
	if container == nil {
		rt.mu.Unlock()
		return
	}
	container.State = "exited"
	container.ExitCode = exitCode
	container.OOMKilled = oomKilled
	container.Error = errMessage
	for _, logs := range container.logs {
		logs.Close()
	}
	rt.mu.Unlock()
}

// ---------------------------------------------------------------------------
// runtime.Runtime streaming implementation
// ---------------------------------------------------------------------------

// LogsFollow streams container output (interleaved when both streams are
// requested; the tailer path uses LogsStreams).
func (rt *FakeRuntime) LogsFollow(ctx context.Context, id string, fromStdout, fromStderr bool) (io.ReadCloser, error) {
	rt.mu.Lock()
	c := rt.get(id)
	if c == nil {
		rt.mu.Unlock()
		return nil, fmt.Errorf("no such container: %s", id)
	}
	so, se := c.logs["stdout"], c.logs["stderr"]
	rt.mu.Unlock()
	switch {
	case fromStdout && !fromStderr:
		return &readCloser{r: &logReader{buf: so}}, nil
	case fromStderr && !fromStdout:
		return &readCloser{r: &logReader{buf: se}}, nil
	default:
		return &readCloser{r: &mergeReader{a: &logReader{buf: so}, b: &logReader{buf: se}}}, nil
	}
}

// LogsStreams returns (stdout, stderr) for the agent's tailer.
func (rt *FakeRuntime) LogsStreams(ctx context.Context, id string) (io.ReadCloser, io.ReadCloser, error) {
	rt.mu.Lock()
	c := rt.get(id)
	if c == nil {
		rt.mu.Unlock()
		return nil, nil, fmt.Errorf("no such container: %s", id)
	}
	so, se := c.logs["stdout"], c.logs["stderr"]
	rt.mu.Unlock()
	return &readCloser{r: &logReader{buf: so}}, &readCloser{r: &logReader{buf: se}}, nil
}

type readCloser struct{ r io.Reader }

func (rc *readCloser) Read(p []byte) (int, error) { return rc.r.Read(p) }
func (rc *readCloser) Close() error               { return nil }

// mergeReader alternates between two readers, honoring EOF on both.
type mergeReader struct {
	a, b  io.Reader
	doneA bool
	doneB bool
}

func (m *mergeReader) Read(p []byte) (int, error) {
	var lastErr error
	for {
		if !m.doneA {
			n, err := m.a.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil {
				m.doneA = true
				lastErr = err
			}
			continue
		}
		if !m.doneB {
			n, err := m.b.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil {
				m.doneB = true
				lastErr = err
			}
			continue
		}
		if lastErr == nil {
			lastErr = io.EOF
		}
		return 0, lastErr
	}
}

var _ = strings.Contains
