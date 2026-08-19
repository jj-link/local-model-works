// Package events is the persisted, resumable event stream: every
// mutation and state change is appended to the events table and fanned
// out to live SSE subscribers.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/jj-link/local-model-works/internal/db"
)

// Event is one stream entry.
type Event struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	NodeID  string          `json:"node_id,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// EventBus persists events and fans them out to live SSE subscribers.
// Publish and Subscribe's cancel hold the same mutex, so a subscriber can
// never be sent to and closed concurrently.
type EventBus struct {
	q      *db.Queries
	mu     sync.Mutex
	subs   map[int64]chan Event
	nextID int64
}

func NewEventBus(q *db.Queries) *EventBus {
	return &EventBus{q: q, subs: map[int64]chan Event{}}
}

// Publish appends an event and notifies live subscribers. nodeID may be "".
func (b *EventBus) Publish(ctx context.Context, typ, nodeID string, payload json.RawMessage) error {
	node := sql.NullString{}
	if nodeID != "" {
		node = sql.NullString{String: nodeID, Valid: true}
	}
	id, err := b.q.AppendEvent(ctx, db.AppendEventParams{Type: typ, NodeID: node, Payload: string(payload)})
	if err != nil {
		return err
	}
	ev := Event{ID: id, Type: typ, NodeID: nodeID, Payload: payload}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than block the publisher
		}
	}
	return nil
}

// Subscribe returns a live event channel plus a cancel. Events with ID after
// afterID (up to 500, the SSE resume window) are replayed first.
func (b *EventBus) Subscribe(ctx context.Context, afterID int64) (<-chan Event, func()) {
	ch := make(chan Event, 1024)
	b.mu.Lock()
	b.nextID++
	subID := b.nextID
	b.subs[subID] = ch
	b.mu.Unlock()

	if afterID < 0 {
		afterID = 0
	}
	if rows, err := b.q.ListEventsSince(ctx, db.ListEventsSinceParams{ID: afterID, Limit: 500}); err == nil {
		for _, r := range rows {
			node := ""
			if r.NodeID.Valid {
				node = r.NodeID.String
			}
			select {
			case ch <- Event{ID: r.ID, Type: r.Type, NodeID: node, Payload: json.RawMessage(r.Payload)}:
			default:
			}
		}
	}

	cancel := func() {
		b.mu.Lock()
		close(ch)
		delete(b.subs, subID)
		b.mu.Unlock()
	}
	return ch, cancel
}
