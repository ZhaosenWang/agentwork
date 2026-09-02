// Package events is the in-process pub/sub bus. Services publish after DB
// writes commit; listeners (WS hub, daemon) react. No cross-node relay in MVP.
package events

import (
	"context"
	"sync"
	"sync/atomic"
)

// Event is a typed notification. Topic routes it; Payload carries the data.
type Event struct {
	Topic   string
	Payload any
}

// Handler reacts to an event.
type Handler func(context.Context, Event)

// handlerEntry pairs a handler with a unique id so unsubscribe can match the
// exact subscription, not the function code pointer (two different Notifier
// instances share the same method code pointer, so reflect.Pointer comparison
// would remove the wrong handler — the orphan-Notifier bug).
type handlerEntry struct {
	id uint64
	h  Handler
}

// Bus is an asynchronous in-process pub/sub: Publish fans each event out to
// the topic's handlers, each running in its own goroutine with panic
// recovery, and returns immediately. Consequences (documented, not bugs):
// handler order across publishes is not guaranteed, and there is no
// acknowledgement. Order-sensitive consumers must not rely on cross-event
// ordering; state transitions themselves are serialized by the DB
// transactions, not by the bus.
type Bus struct {
	mu       sync.RWMutex
	handlers map[string][]handlerEntry
	nextID   atomic.Uint64
}

func NewBus() *Bus {
	return &Bus{handlers: make(map[string][]handlerEntry)}
}

// Subscribe registers a handler for a topic. Returns an unsubscribe func.
func (b *Bus) Subscribe(topic string, h Handler) func() {
	id := b.nextID.Add(1)
	b.mu.Lock()
	b.handlers[topic] = append(b.handlers[topic], handlerEntry{id: id, h: h})
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		hs := b.handlers[topic]
		for i, e := range hs {
			if e.id == id {
				b.handlers[topic] = append(hs[:i], hs[i+1:]...)
				return
			}
		}
	}
}

// Publish fans an event to all handlers for its topic. Each handler runs in
// its own goroutine; Publish returns immediately.
func (b *Bus) Publish(ctx context.Context, e Event) {
	b.mu.RLock()
	hs := b.handlers[e.Topic]
	b.mu.RUnlock()
	for _, entry := range hs {
		go func(h Handler) {
			defer func() { _ = recover() }()
			h(ctx, e)
		}(entry.h)
	}
}
