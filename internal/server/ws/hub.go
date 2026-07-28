// Package ws is the WebSocket boundary. A Hub subscribes to all events.Bus
// topics and broadcasts each event as {"topic":..., "payload":...} JSON to
// every connected client. MVP is fan-out only: no per-connection filtering,
// no RPC, no auth. The frontend subscribes to the stream and filters by
// payload.task_id itself.
package ws

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/eushing/agentwork/internal/events"
)

// topics is the full set of bus topics the hub forwards to clients. New
// topics added to the system must be listed here or they won't reach the
// frontend.
var topics = []string{
	"task:created", "task:assigned", "task:finished", "task:retrying", "task:deleted",
	"task:message", "task:thought", "task:tool",
	"task:waiting", "task:wakeup",
	"agent:created", "agent:deleted",
	"schedule:created", "schedule:fired",
}

// Hub owns the set of active WS clients and fans out bus events to them.
type Hub struct {
	bus     *events.Bus
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func NewHub(bus *events.Bus) *Hub {
	return &Hub{bus: bus, clients: make(map[*Client]struct{})}
}

// Run subscribes to every topic and blocks until ctx is cancelled. Call in a
// goroutine at server startup.
func (h *Hub) Run(ctx context.Context) {
	for _, topic := range topics {
		h.bus.Subscribe(topic, func(_ context.Context, e events.Event) {
			msg, err := json.Marshal(map[string]any{"topic": e.Topic, "payload": e.Payload})
			if err != nil {
				log.Printf("ws: marshal event %s: %v", e.Topic, err)
				return
			}
			h.broadcast(msg)
		})
	}
	<-ctx.Done()
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// broadcast sends msg to every client's send channel. Non-blocking: a slow
// client whose send buffer is full is dropped (its readPump will still tear it
// down on the next failed write). Single-user local use makes this path rare.
func (h *Hub) broadcast(msg []byte) {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// drop on full; better to lose a message than block the bus.
		}
	}
	h.mu.Unlock()
}
