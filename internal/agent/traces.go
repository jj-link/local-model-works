package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/traces"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const (
	traceSocketPath = "/run/lmw/trace.sock"
	traceSource     = "openhands"
	traceChunkBytes = 64 << 10
)

type traceSpoolManager struct {
	a      *Agent
	mu     sync.Mutex
	spools map[string]*traceSpool
}

type traceSpool struct {
	manager   *traceSpoolManager
	mu        sync.Mutex
	runID     string
	rank      int32
	source    string
	dir       string
	path      string
	socket    string
	file      *os.File
	listener  net.Listener
	redactor  traces.Redactor
	secrets   map[string]string
	size      uint64
	committed uint64
	final     bool
	inflight  bool
}

type traceSpoolState struct {
	RunID     string `json:"run_id"`
	Rank      int32  `json:"rank"`
	Source    string `json:"source"`
	Size      uint64 `json:"size"`
	Committed uint64 `json:"committed"`
	Final     bool   `json:"final"`
}

func newTraceSpoolManager(a *Agent) *traceSpoolManager {
	return &traceSpoolManager{a: a, spools: map[string]*traceSpool{}}
}

func traceSpoolKey(runID string, rank int32, source string) string {
	return fmt.Sprintf("%s:%d:%s", runID, rank, source)
}

func (m *traceSpoolManager) root() string { return filepath.Join(m.a.cfg.StateRoot, "traces") }

func (m *traceSpoolManager) start(wc *agentv1.WorkloadCommand, spec *runtime.ContainerSpec) error {
	if wc.GetTraceSchema() != traces.SchemaVersion {
		return fmt.Errorf("trace.schema_unsupported: %s", wc.GetTraceSchema())
	}
	if wc.GetTraceSocket() != traceSocketPath {
		return fmt.Errorf("trace.socket_invalid: %s", wc.GetTraceSocket())
	}
	key := traceSpoolKey(wc.GetRunId(), wc.GetRank(), traceSource)
	m.mu.Lock()
	if existing := m.spools[key]; existing != nil {
		m.mu.Unlock()
		if !hasTraceMount(spec.Mounts, existing.dir) {
			spec.Mounts = append(spec.Mounts, runtime.MountSpec{Source: existing.dir, Dest: "/run/lmw"})
		}
		return nil
	}
	dir := filepath.Join(m.root(), shortID(wc.GetRunId())+fmt.Sprintf("-r%d", wc.GetRank()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.mu.Unlock()
		return err
	}
	path := filepath.Join(dir, "trace.ndjson")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		m.mu.Unlock()
		return err
	}
	secretValues := make([]string, 0, len(wc.GetSecrets()))
	secrets := make(map[string]string, len(wc.GetSecrets()))
	for name, value := range wc.GetSecrets() {
		secrets[name] = value
		secretValues = append(secretValues, value)
	}
	spool := &traceSpool{manager: m, runID: wc.GetRunId(), rank: wc.GetRank(), source: traceSource,
		dir: dir, path: path, socket: filepath.Join(dir, "trace.sock"), file: file,
		redactor: traces.NewRedactor(secretValues...), secrets: secrets, size: uint64(info.Size())}
	m.spools[key] = spool
	m.mu.Unlock()

	if err := os.Remove(spool.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.remove(key, spool)
		return err
	}
	listener, err := net.Listen("unix", spool.socket)
	if err != nil {
		m.remove(key, spool)
		return err
	}
	if err := os.Chmod(spool.socket, 0o600); err != nil {
		listener.Close()
		m.remove(key, spool)
		return err
	}
	spool.listener = listener
	if err := spool.persist(); err != nil {
		listener.Close()
		m.remove(key, spool)
		return err
	}
	spec.Mounts = append(spec.Mounts, runtime.MountSpec{Source: dir, Dest: "/run/lmw"})
	go spool.accept()
	return nil
}

func hasTraceMount(mounts []runtime.MountSpec, source string) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Dest == "/run/lmw" {
			return true
		}
	}
	return false
}

func (s *traceSpool) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.consume(conn)
	}
}

func (s *traceSpool) consume(conn net.Conn) {
	defer conn.Close()
	// The first server-to-runner record is memory-only credential delivery.
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"type": "localmodelworks/credentials/v1", "values": s.secrets,
	}); err != nil {
		return
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var raw map[string]any
		decoder := json.NewDecoder(bytesReader(scanner.Bytes()))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			continue
		}
		clean, count := s.redactor.Redact(raw)
		if envelope, ok := clean.(map[string]any); ok {
			envelope["redaction_count"] = count
		}
		line, err := json.Marshal(clean)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		if err := s.append(line); err != nil {
			return
		}
	}
}

// bytesReader avoids retaining Scanner's backing array after Decode returns.
func bytesReader(data []byte) io.Reader {
	copyOfData := append([]byte(nil), data...)
	return &byteReader{data: copyOfData}
}

type byteReader struct{ data []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (s *traceSpool) append(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return errors.New("trace spool finalized")
	}
	n, err := s.file.Write(line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.size += uint64(n)
	if err := s.persistLocked(); err != nil {
		return err
	}
	s.sendLocked()
	return nil
}

func (m *traceSpoolManager) sanitizeLog(runID string, rank int32, data []byte) []byte {
	m.mu.Lock()
	spool := m.spools[traceSpoolKey(runID, rank, traceSource)]
	m.mu.Unlock()
	if spool == nil {
		return data
	}
	spool.mu.Lock()
	clean, _ := spool.redactor.Redact(string(data))
	spool.mu.Unlock()
	text, ok := clean.(string)
	if !ok {
		return []byte("[REDACTED:credential]\n")
	}
	return []byte(text)
}

func (m *traceSpoolManager) enabled(runID string, rank int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spools[traceSpoolKey(runID, rank, traceSource)] != nil
}

func (m *traceSpoolManager) finalize(runID string, rank int32) error {
	key := traceSpoolKey(runID, rank, traceSource)
	m.mu.Lock()
	spool := m.spools[key]
	m.mu.Unlock()
	if spool == nil {
		return errors.New("trace.spool_missing")
	}
	spool.mu.Lock()
	defer spool.mu.Unlock()
	if spool.listener != nil {
		_ = spool.listener.Close()
		spool.listener = nil
	}
	spool.final = true
	for name := range spool.secrets {
		delete(spool.secrets, name)
	}
	if err := spool.persistLocked(); err != nil {
		return err
	}
	spool.sendLocked()
	return nil
}

func (s *traceSpool) sendLocked() {
	if s.inflight {
		return
	}
	if s.committed < s.size {
		remaining := s.size - s.committed
		if remaining > traceChunkBytes {
			remaining = traceChunkBytes
		}
		data := make([]byte, remaining)
		if _, err := s.file.ReadAt(data, int64(s.committed)); err != nil && !errors.Is(err, io.EOF) {
			return
		}
		s.inflight = true
		s.manager.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_TraceChunk{TraceChunk: &agentv1.TraceChunk{
			RunId: s.runID, Rank: s.rank, Source: s.source, Offset: s.committed, Data: data,
		}}})
		return
	}
	if s.final {
		s.inflight = true
		s.manager.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_TraceChunk{TraceChunk: &agentv1.TraceChunk{
			RunId: s.runID, Rank: s.rank, Source: s.source, Offset: s.size, Final: true, EndOffset: s.size,
		}}})
	}
}

func (m *traceSpoolManager) ack(ack *agentv1.TraceAck) {
	key := traceSpoolKey(ack.GetRunId(), ack.GetRank(), ack.GetSource())
	m.mu.Lock()
	spool := m.spools[key]
	m.mu.Unlock()
	if spool == nil {
		return
	}
	spool.mu.Lock()
	spool.inflight = false
	if ack.GetError() != "" {
		spool.mu.Unlock()
		return
	}

	if ack.GetCommittedOffset() < spool.committed || ack.GetCommittedOffset() > spool.size {
		spool.mu.Unlock()
		return
	}
	spool.committed = ack.GetCommittedOffset()
	if err := spool.persistLocked(); err != nil {
		spool.mu.Unlock()
		return
	}
	remove := ack.GetFinal() && spool.final && spool.committed == spool.size
	if !remove {
		spool.sendLocked()
	}
	spool.mu.Unlock()
	if remove {
		m.remove(key, spool)
	}
}

func (m *traceSpoolManager) resendAll() {
	m.mu.Lock()
	spools := make([]*traceSpool, 0, len(m.spools))
	for _, spool := range m.spools {
		spools = append(spools, spool)
	}
	m.mu.Unlock()
	for _, spool := range spools {
		spool.mu.Lock()
		spool.inflight = false
		spool.sendLocked()
		spool.mu.Unlock()
	}
}

func (s *traceSpool) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

func (s *traceSpool) persistLocked() error {
	data, err := json.Marshal(traceSpoolState{RunID: s.runID, Rank: s.rank, Source: s.source, Size: s.size, Committed: s.committed, Final: s.final})
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

func (m *traceSpoolManager) remove(key string, spool *traceSpool) {
	m.mu.Lock()
	if m.spools[key] == spool {
		delete(m.spools, key)
	}
	m.mu.Unlock()
	if spool.listener != nil {
		_ = spool.listener.Close()
	}
	if spool.file != nil {
		_ = spool.file.Close()
	}
	_ = os.RemoveAll(spool.dir)
}

func (m *traceSpoolManager) load() error {
	if err := os.MkdirAll(m.root(), 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(m.root())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(m.root(), entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			continue
		}
		var state traceSpoolState
		if json.Unmarshal(data, &state) != nil || state.RunID == "" || state.Source == "" || state.Committed > state.Size {
			continue
		}
		file, err := os.OpenFile(filepath.Join(dir, "trace.ndjson"), os.O_RDWR|os.O_APPEND, 0o600)
		if err != nil {
			continue
		}
		spool := &traceSpool{manager: m, runID: state.RunID, rank: state.Rank, source: state.Source,
			dir: dir, path: filepath.Join(dir, "trace.ndjson"), socket: filepath.Join(dir, "trace.sock"), file: file,
			redactor: traces.NewRedactor(), secrets: map[string]string{}, size: state.Size, committed: state.Committed, final: state.Final}
		m.spools[traceSpoolKey(state.RunID, state.Rank, state.Source)] = spool
	}
	return nil
}
