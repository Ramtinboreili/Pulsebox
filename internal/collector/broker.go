package collector

import (
	"sync"

	"github.com/Ramtinboreili/Pulsebox/internal/topology"
)

// broker fans topology diffs out to connected WebSocket subscribers. A slow
// subscriber whose buffer fills is dropped (its channel closed); the client is
// expected to reconnect and receive a fresh snapshot.
type broker struct {
	mu   sync.Mutex
	subs map[int]chan topology.Diff
	next int
}

func newBroker() *broker {
	return &broker{subs: map[int]chan topology.Diff{}}
}

// subscribe registers a new subscriber and returns its id and channel.
func (b *broker) subscribe() (int, chan topology.Diff) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan topology.Diff, 32)
	b.subs[id] = ch
	return id, ch
}

// unsubscribe removes and closes a subscriber.
func (b *broker) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// broadcast delivers a diff to every subscriber, dropping any that are full.
func (b *broker) broadcast(d topology.Diff) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		select {
		case ch <- d:
		default:
			// Subscriber is not keeping up; drop it so it reconnects fresh.
			delete(b.subs, id)
			close(ch)
		}
	}
}

func (b *broker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
