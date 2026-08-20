// Package commands is the core command-result broker: agents acknowledge
// workload commands asynchronously with a CommandResult keyed by command
// ID. The broker lets core services and module job executors await a
// specific result (with a replay window for results that arrive before the
// waiter attaches) instead of inferring success from logs.
package commands

import (
	"container/list"
	"sync"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// replaySize bounds the window of already-delivered results that late
// waiters can still see (send-then-wait races, reconnects).
const replaySize = 1024

// Broker routes agent CommandResult messages to waiters keyed by command
// ID. Deliver is safe from any goroutine; each result is delivered to a
// given waiter at most once.
type Broker struct {
	mu      sync.Mutex
	waiters map[string][]chan *agentv1.CommandResult
	replay  map[string]*agentv1.CommandResult
	order   *list.List
}

// New builds an empty broker.
func New() *Broker {
	return &Broker{
		waiters: map[string][]chan *agentv1.CommandResult{},
		replay:  map[string]*agentv1.CommandResult{},
		order:   list.New(),
	}
}

// Wait registers interest in the result of commandID and returns a channel
// that delivers it exactly once, plus a release function. Callers MUST call
// release (typically via defer) so a timed-out or abandoned wait does not
// leak its waiter entry. If the result was already delivered (and is still
// in the replay window), it is received immediately and release is a no-op.
// The channel is never closed; callers bound it with a timeout, then
// release.
func (b *Broker) Wait(commandID string) (<-chan *agentv1.CommandResult, func()) {
	ch := make(chan *agentv1.CommandResult, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if cr, ok := b.replay[commandID]; ok {
		ch <- cr
		return ch, func() {}
	}
	b.waiters[commandID] = append(b.waiters[commandID], ch)
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeWaiter(commandID, ch)
	}
}

// removeWaiter drops ch from commandID's waiter list (caller holds mu).
func (b *Broker) removeWaiter(commandID string, ch chan *agentv1.CommandResult) {
	list := b.waiters[commandID]
	out := list[:0]
	for _, c := range list {
		if c != ch {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(b.waiters, commandID)
	} else {
		b.waiters[commandID] = out
	}
}

// Deliver routes one result to all registered waiters for its command ID.
// Results are retained in the replay window for late waiters; unknown
// command IDs (already consumed or foreign) cost only the window slot.
func (b *Broker) Deliver(cr *agentv1.CommandResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replay[cr.CommandId] = cr
	b.order.PushBack(cr.CommandId)
	for b.order.Len() > replaySize {
		oldest := b.order.Front()
		b.order.Remove(oldest)
		delete(b.replay, oldest.Value.(string))
	}
	for _, ch := range b.waiters[cr.CommandId] {
		ch <- cr
	}
	delete(b.waiters, cr.CommandId)
}
