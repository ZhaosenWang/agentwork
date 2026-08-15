package notify

import (
	"strings"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func strPtr(s string) *string { return &s }

// TestDispatchInboundRejectsNonText: a post (rich text) message must NOT be
// silently dropped — the owner gets a "暂时不支持富文本消息" reply. Previously
// every non-text message died quietly (no ⏳, no error), which looked like a
// broken bot.
func TestDispatchInboundRejectsNonText(t *testing.T) {
	sent := make(chan string, 1)
	conn := &Connector{
		notify: &Notifier{
			send: func(text string) error { sent <- text; return nil },
		},
	}
	conn.dispatchInbound(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageType: strPtr("post"),
				ChatType:    strPtr("p2p"),
				Content:     strPtr(`{"zh_cn":{"content":[[{"tag":"text","text":"hi"}]]}}`),
			},
		},
	}, "ou_owner")

	select {
	case got := <-sent:
		if !strings.Contains(got, "不支持") {
			t.Fatalf("expected a 不支持 hint, got: %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("non-text message was silently dropped — expected a rejection reply")
	}
}

// TestDispatchInboundNilEvent guards the nil-entry early return (no panic).
func TestDispatchInboundNilEvent(t *testing.T) {
	conn := &Connector{}
	conn.dispatchInbound(nil, "")
	conn.dispatchInbound(&larkim.P2MessageReceiveV1{}, "")
}
