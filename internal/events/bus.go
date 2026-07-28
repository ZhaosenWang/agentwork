// Package events is the in-process pub/sub bus. Services publish after DB
// writes commit; listeners (WS hub, daemon) react. No cross-node relay in MVP.
package events

import (
	"context"
	"reflect"
	"sync"
)

// Event is a typed notification. Topic routes it; Payload carries the data.
type Event struct {
	Topic   string
	Payload any
}

// Handler reacts to an event.
type Handler func(context.Context, Event)

// Bus is a synchronous in-process pub/sub. Each handler runs in its own
// goroutine with panic recovery so one bad listener can't take down the rest.
type Bus struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
}

func NewBus() *Bus {
	return &Bus{handlers: make(map[string][]Handler)}
}

// Subscribe registers a handler for a topic. Returns an unsubscribe func.
func (b *Bus) Subscribe(topic string, h Handler) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = append(b.handlers[topic], h)
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		hs := b.handlers[topic]
		for i, hh := range hs {
			// Compare by pointer identity. The range loop copies each element
			// into hh, so we must take the address of the slice slot, not hh.
			if reflect.ValueOf(hh).Pointer() == reflect.ValueOf(h).Pointer() {
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
	for _, h := range hs {
		go func(h Handler) {
			defer func() { _ = recover() }()
			h(ctx, e)
		}(h)
	}
}
