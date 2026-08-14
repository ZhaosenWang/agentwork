package ws

import (
	"net/http"

	"github.com/eushing/agentwork/internal/logging"
	"github.com/gorilla/websocket"
)

const (
	upgraderReadBufferSize  = 1024
	upgraderWriteBufferSize = 1024
	clientSendBuffer        = 64
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  upgraderReadBufferSize,
	WriteBufferSize: upgraderWriteBufferSize,
	CheckOrigin: func(r *http.Request) bool {
		return true // single-user local; no origin restriction
	},
}

// ServeWS upgrades one HTTP connection to WS, registers it with the hub, and
// pumps until the client disconnects.
func ServeWS(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logging.Errorf("ws: upgrade: %v", err)
		return
	}
	c := &Client{hub: hub, conn: conn, send: make(chan []byte, clientSendBuffer)}
	hub.register(c)
	go c.writePump()
	c.readPump()
}
