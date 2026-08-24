package agonrunner

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/id"
)

const eventVersion = 1

var eventTypes = map[string]bool{
	"run.status": true, "phase.changed": true, "agent.started": true, "agent.text.delta": true,
	"agent.tool.started": true, "agent.tool.finished": true, "agent.usage": true, "agent.finished": true,
	"advisor.started": true, "advisor.note": true, "advisor.finished": true, "artifact.changed": true,
	"decision.required": true, "error": true,
}

// Event is the versioned public worker stream contract.
type Event struct {
	Version            int            `json:"version"`
	EventID            string         `json:"event_id"`
	RunID              string         `json:"run_id"`
	InvocationID       string         `json:"invocation_id,omitempty"`
	ParentInvocationID string         `json:"parent_invocation_id,omitempty"`
	NodeID             string         `json:"node_id,omitempty"`
	Timestamp          time.Time      `json:"timestamp"`
	Type               string         `json:"type"`
	Payload            map[string]any `json:"payload"`
}

// EventSink accepts complete normalized events.
type EventSink interface {
	Emit(Event) error
}

type ndjsonSink struct {
	mu     sync.Mutex
	writer io.Writer
}

func newNDJSONSink(writer io.Writer) EventSink { return &ndjsonSink{writer: writer} }

func (s *ndjsonSink) Emit(event Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.writer.Write(append(encoded, '\n'))
	return err
}

// Obfuscator removes every mounted secret before data enters a public event.
type Obfuscator struct{ secrets []string }

func LoadObfuscator(directory string) *Obfuscator {
	obfuscator := &Obfuscator{}
	if directory == "" {
		return obfuscator
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return obfuscator
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			if value, err := os.ReadFile(filepath.Join(directory, entry.Name())); err == nil && len(value) > 0 {
				obfuscator.secrets = append(obfuscator.secrets, string(value))
			}
		}
	}
	sort.Slice(obfuscator.secrets, func(i, j int) bool { return len(obfuscator.secrets[i]) > len(obfuscator.secrets[j]) })
	return obfuscator
}

func (o *Obfuscator) String(value string) string {
	for _, secret := range o.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return value
}

func (o *Obfuscator) Value(value any) any {
	switch typed := value.(type) {
	case string:
		return o.String(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = o.Value(typed[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "thinking") || strings.Contains(lower, "reasoning") {
				continue
			}
			out[key] = o.Value(child)
		}
		return out
	default:
		return value
	}
}

// Emitter fills event identity and applies redaction.
type Emitter struct {
	Sink               EventSink
	RunID              string
	InvocationID       string
	ParentInvocationID string
	NodeID             string
	Obfuscator         *Obfuscator
}

func (e *Emitter) Emit(eventType string, payload map[string]any) error {
	if e == nil || e.Sink == nil || !eventTypes[eventType] {
		return fmt.Errorf("autoresearch.event_invalid: %s", eventType)
	}
	eventID, err := id.New()
	if err != nil {
		return err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if e.Obfuscator != nil {
		payload = e.Obfuscator.Value(payload).(map[string]any)
	}
	return e.Sink.Emit(Event{
		Version: eventVersion, EventID: eventID, RunID: e.RunID, InvocationID: e.InvocationID,
		ParentInvocationID: e.ParentInvocationID, NodeID: e.NodeID, Timestamp: time.Now().UTC(),
		Type: eventType, Payload: payload,
	})
}

type framedSink struct {
	mu   sync.Mutex
	conn net.Conn
}

func newFramedSink(path string) (EventSink, io.Closer, error) {
	connection, err := net.Dial("unix", path)
	if err != nil {
		return nil, nil, err
	}
	sink := &framedSink{conn: connection}
	return sink, connection, nil
}

func (s *framedSink) Emit(event Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if len(encoded) > 1<<20 {
		return errors.New("autoresearch.event_too_large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.conn.Write(header[:]); err != nil {
		return err
	}
	_, err = s.conn.Write(encoded)
	return err
}

// SocketServer receives framed events from concurrent nested role processes.
type SocketServer struct {
	listener net.Listener
	sink     EventSink
	wg       sync.WaitGroup
}

func StartSocketServer(path string, sink EventSink) (*SocketServer, error) {
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	server := &SocketServer{listener: listener, sink: sink}
	server.wg.Add(1)
	go server.accept()
	return server, nil
}

func (s *SocketServer) accept() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.read(connection)
	}
}

func (s *SocketServer) read(connection net.Conn) {
	defer s.wg.Done()
	defer connection.Close()
	reader := bufio.NewReader(connection)
	for {
		var header [4]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return
		}
		size := binary.BigEndian.Uint32(header[:])
		if size == 0 || size > 1<<20 {
			return
		}
		encoded := make([]byte, size)
		if _, err := io.ReadFull(reader, encoded); err != nil {
			return
		}
		event := Event{}
		if json.Unmarshal(encoded, &event) != nil || event.Version != eventVersion || !eventTypes[event.Type] {
			continue
		}
		_ = s.sink.Emit(event)
	}
}

func (s *SocketServer) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

func roleNode(role string) string {
	represented := map[string]string{
		"idea-creator": "idea.creator", "idea-refiner": "idea.refiner", "idea-reviewer": "idea.reviewer",
		"proposal-refiner": "proposal.refiner", "proposal-reviewer": "proposal.reviewer", "deep-lit-reader": "literature.reader",
		"experiment-scientist": "experiment.scientist", "experiment-coder": "experiment.coder", "experiment-auditor": "experiment.auditor", "experiment-reviewer": "experiment.reviewer",
		"paper-writer": "paper.writer", "paper-auditor-evidence": "paper.evidence", "paper-auditor-citations": "paper.citations",
		"paper-auditor-reproducibility": "paper.reproducibility", "paper-rhetorician": "paper.rhetorician", "paper-reviewer": "paper.reviewer",
		"paper-killer-reviewer": "paper.killer", "paper-area-chair": "paper.area-chair",
	}
	if node, ok := represented[role]; ok {
		return node
	}
	return "aux:" + role
}
