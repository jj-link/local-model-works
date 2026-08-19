package server

import (
	"sync"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// NodeConn is one live agent session: an outbound message channel plus a
// done flag. Send is safe after Close (it reports false instead of panicking
// on a closed channel).
type NodeConn struct {
	NodeID    string
	SendCh    chan *agentv1.ServerMessage
	done      chan struct{}
	closeOnce sync.Once
}

func newNodeConn(nodeID string) *NodeConn {
	return &NodeConn{
		NodeID: nodeID,
		SendCh: make(chan *agentv1.ServerMessage, 64),
		done:   make(chan struct{}),
	}
}

// Send queues a server message. It returns false once the session is
// closing or has closed.
func (c *NodeConn) Send(m *agentv1.ServerMessage) bool {
	select {
	case <-c.done:
		return false
	case c.SendCh <- m:
		return true
	}
}

// Close marks the session finished. Safe to call more than once.
func (c *NodeConn) Close() { c.closeOnce.Do(func() { close(c.done) }) }

// NodeRegistry tracks the live agent sessions, one per node.
type NodeRegistry struct {
	mu    sync.Mutex
	conns map[string]*NodeConn
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{conns: map[string]*NodeConn{}}
}

// Register stores conn; a previous session for the same node is closed.
func (r *NodeRegistry) Register(conn *NodeConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.conns[conn.NodeID]; ok && old != conn {
		old.Close()
	}
	r.conns[conn.NodeID] = conn
}

// Unregister removes conn if it is still the current session for its node.
func (r *NodeRegistry) Unregister(conn *NodeConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[conn.NodeID] == conn {
		delete(r.conns, conn.NodeID)
	}
}

// Send delivers a server message to the node's live session, if any.
func (r *NodeRegistry) Send(nodeID string, m *agentv1.ServerMessage) bool {
	r.mu.Lock()
	conn := r.conns[nodeID]
	r.mu.Unlock()
	return conn != nil && conn.Send(m)
}

// Online reports whether the node currently has a live session.
func (r *NodeRegistry) Online(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.conns[nodeID] != nil
}

// OnlineCount is the number of nodes with a live session.
func (r *NodeRegistry) OnlineCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
