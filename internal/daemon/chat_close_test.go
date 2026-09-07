package daemon

import (
	"testing"
)

// TestCloseChatClosesWebSocket: the web-side close path must close the web
// socket, mirroring MachineChatClosed. CloseChat used to skip e.close(),
// so a ChatWrite error (handler return → defer CloseChat) left the socket
// open and the frontend's prompt Promise hung ~5 min until the proxy idle
// timeout dropped the silent socket. Fix C adds e.close() here. The call
// is guarded by closeOnce, so a second CloseChat (the deferred one racing
// an explicit close) must not double-close `done` (a panic) or call close
// twice.
func TestCloseChatClosesWebSocket(t *testing.T) {
	d := &Daemon{chat: chatRelay{chats: map[string]*chatEntry{}}}
	closes := 0
	e := &chatEntry{
		machineID: "m1",
		done:      make(chan struct{}),
		close:     func() { closes++ },
	}
	d.chat.chats["chat-1"] = e

	d.CloseChat("chat-1")
	d.CloseChat("chat-1") // second call: entry already gone, closeOnce guards done

	if closes != 1 {
		t.Fatalf("e.close called %d times, want exactly 1 (guarded by closeOnce)", closes)
	}
	select {
	case <-e.done:
	default:
		t.Fatal("CloseChat did not signal done")
	}
	if _, ok := d.chat.chats["chat-1"]; ok {
		t.Fatal("CloseChat left the chat entry in the map")
	}
}

// TestCloseChatNoEntryIsNoop: closing an unknown chat id must not panic
// (CloseChat is deferred on every handler, including ones that failed at
// OpenChatForAgent before any entry was created).
func TestCloseChatNoEntryIsNoop(t *testing.T) {
	d := &Daemon{chat: chatRelay{chats: map[string]*chatEntry{}}}
	d.CloseChat("never-opened") // must not panic
}
