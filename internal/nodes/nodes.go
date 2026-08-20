// Package nodes is the live agent session registry: one outbound message
// channel per connected node. The server owns the sessions; core services
// and module backends send workload, transfer, and log commands through it.
package nodes

import (
	"sync"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// Conn is one live agent session: an outbound message channel plus a done
// flag. Send is safe after Close (it reports false instead of panicking on
// a closed channel).
type Conn struct {
	NodeID    string
	SendCh    chan *agentv1.ServerMessage
	done      chan struct{}
	closeOnce sync.Once
}

// NewConn builds a session channel for nodeID.
func NewConn(nodeID string) *Conn {
	return &Conn{
		NodeID: nodeID,
		SendCh: make(chan *agentv1.ServerMessage, 64),
		done:   make(chan struct{}),
	}
}

// Send queues a server message. It returns false once the session is
// closing or has closed.
func (c *Conn) Send(m *agentv1.ServerMessage) bool {
	select {
	case <-c.done:
		return false
	case c.SendCh <- m:
		return true
	}
}

// Done returns a channel closed when the session ends; select on it to stop
// outbound pumps.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close marks the session finished. Safe to call more than once.
func (c *Conn) Close() { c.closeOnce.Do(func() { close(c.done) }) }

// Registry tracks the live agent sessions, one per node.
type Registry struct {
	mu    sync.Mutex
	conns map[string]*Conn
}

// NewRegistry builds an empty session registry.
func NewRegistry() *Registry {
	return &Registry{conns: map[string]*Conn{}}
}

// Register stores conn; a previous session for the same node is closed.
func (r *Registry) Register(conn *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.conns[conn.NodeID]; ok && old != conn {
		old.Close()
	}
	r.conns[conn.NodeID] = conn
}

// Unregister removes conn if it is still the current session for its node.
func (r *Registry) Unregister(conn *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[conn.NodeID] == conn {
		delete(r.conns, conn.NodeID)
	}
}

// Send delivers a server message to the node's live session, if any.
func (r *Registry) Send(nodeID string, m *agentv1.ServerMessage) bool {
	r.mu.Lock()
	conn := r.conns[nodeID]
	r.mu.Unlock()
	return conn != nil && conn.Send(m)
}

// Broadcast queues a message for every currently connected node.
func (r *Registry) Broadcast(message *agentv1.ServerMessage) int {
	r.mu.Lock()
	connections := make([]*Conn, 0, len(r.conns))
	for _, connection := range r.conns {
		connections = append(connections, connection)
	}
	r.mu.Unlock()
	sent := 0
	for _, connection := range connections {
		if connection.Send(message) {
			sent++
		}
	}
	return sent
}

// Online reports whether the node currently has a live session.
func (r *Registry) Online(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[nodeID] != nil
}

// OnlineCount is the number of nodes with a live session.
func (r *Registry) OnlineCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
