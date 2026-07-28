package ws

import (
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second // well under pongWait so ping reliably refreshes the read deadline
	readLimit  = 4096              // max incoming message size; MVP is server→client only so this just bounds junk
)

// Client is one WS connection. readPump runs in the handler goroutine;
// writePump runs in its own goroutine. send is closed by unregister.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// readPump discards incoming messages (MVP is server→client only) and keeps
// the connection alive via pong deadlines. Runs until the connection closes,
// then unregisters.
func (c *Client) readPump() {
	defer c.conn.Close()
	c.conn.SetReadLimit(readLimit)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
	c.hub.unregister(c)
}

// writePump drains c.send to the connection and pings on a timer. Exits when
// send is closed (by unregister) or a write fails.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
