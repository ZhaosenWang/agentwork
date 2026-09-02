package daemon

// Steward chat interception: when the steward agent is used in the ACP chat
// surface (not the intake processor run), the daemon relay buffers its text
// output, scans for the <<<INTAKE_JSON>>>{...}<<<END>>> marker on turn
// completion, dispatches the parsed action through intakeReg (the same
// handlers as the IM/Web intake path), and injects synthetic ACP frames
// (cleaned reply + dispatch result + prompt response) to the web.
//
// This file contains the steward-specific helpers; the interception points
// (ChatWrite, MachineChatFrame, BindChatSink) live in chat.go.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/eushing/agentwork/internal/logging"
)

const (
	intakeJSONStart = "<<<INTAKE_JSON>>>"
	intakeJSONEnd   = "<<<END>>>"
)

// extractIntakeMarker scans the steward's accumulated text for the marker.
// Returns the JSON payload, the clean display text (everything before the
// marker), and whether the marker was found. A partial marker (missing END)
// is treated as not found — the steward was likely interrupted mid-output.
func extractIntakeMarker(text string) (jsonStr, cleanText string, found bool) {
	start := strings.Index(text, intakeJSONStart)
	if start < 0 {
		return "", text, false
	}
	rest := text[start+len(intakeJSONStart):]
	end := strings.Index(rest, intakeJSONEnd)
	if end < 0 {
		return "", text, false
	}
	jsonStr = strings.TrimSpace(rest[:end])
	cleanText = strings.TrimSpace(text[:start])
	return jsonStr, cleanText, true
}

// classifyStewardFrame determines how a machine→web frame should be handled
// in a steward chat. Returns "buffer" (accumulate, don't forward), "complete"
// (turn ended, trigger completion), or "forward" (pass through to the web).
func classifyStewardFrame(frame []byte, pendingPromptID string) string {
	var probe struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return "forward"
	}
	if probe.Method == "" && len(probe.ID) > 0 {
		var idStr string
		_ = json.Unmarshal(probe.ID, &idStr)
		if idStr == pendingPromptID {
			return "complete"
		}
		return "forward"
	}
	if probe.Method == "session/update" {
		return "buffer"
	}
	return "forward"
}

// extractUpdateText pulls the text delta from a session/update notification.
// Only agent_message_chunk and agent_thought_chunk carry displayable text;
// tool_call and tool_call_update are buffered as empty strings (their
// content is structured, not part of the steward's text reply).
func extractUpdateText(frame []byte) string {
	var probe struct {
		Params struct {
			Update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Content       json.RawMessage `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return ""
	}
	switch probe.Params.Update.SessionUpdate {
	case "agent_message_chunk":
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(probe.Params.Update.Content, &c)
		return c.Text
	case "agent_thought_chunk":
		var s string
		_ = json.Unmarshal(probe.Params.Update.Content, &s)
		return s
	default:
		return ""
	}
}

// interceptStewardPrompt extracts the JSON-RPC id and sessionId from a
// session/prompt frame and stores them in the chat entry for later
// response matching. Called from ChatWrite (web→machine leg).
func interceptStewardPrompt(e *chatEntry, frame []byte) {
	var probe struct {
		ID     json.RawMessage `json:"id,omitempty"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return
	}
	if probe.Method != "session/prompt" {
		return
	}
	var idStr string
	_ = json.Unmarshal(probe.ID, &idStr)
	e.mu.Lock()
	e.pendingPromptID = idStr
	e.sessionID = probe.Params.SessionID
	e.stewardFrames = e.stewardFrames[:0]
	e.mu.Unlock()
}

// handleStewardTurnComplete processes the steward's buffered text after a
// turn ends (the prompt response arrived). It sorts the buffered chunks by
// seq (the peer's readLoop dispatches each frame on its own goroutine —
// chunks arrive in random order), concatenates them, extracts the intake
// marker, dispatches the parsed action through intakeReg, and injects
// synthetic ACP frames (cleaned text + dispatch result + prompt response)
// to the web. Runs in a goroutine to avoid blocking the peer's read loop.
func (d *Daemon) handleStewardTurnComplete(chatID string, e *chatEntry) {
	e.mu.Lock()
	frames := e.stewardFrames
	promptID := e.pendingPromptID
	sessionID := e.sessionID
	e.stewardFrames = e.stewardFrames[:0]
	e.pendingPromptID = ""
	e.mu.Unlock()

	go func() {
		sort.Slice(frames, func(i, j int) bool { return frames[i].seq < frames[j].seq })
		var b strings.Builder
		for _, f := range frames {
			b.WriteString(f.text)
		}
		rawText := b.String()

		jsonStr, cleanText, found := extractIntakeMarker(rawText)

		displayText := strings.TrimSpace(cleanText)
		if displayText != "" {
			injectAgentMessage(e, sessionID, displayText)
		}

		if found {
			var parsed intakeAction
			if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
				logging.Infof("chat: %s steward parse failed: %s\nraw: %s", chatID, err.Error(), rawText)
				injectAgentMessage(e, sessionID, "⚠️ 指令解析失败："+err.Error())
			} else {
				reply := intakeReg.dispatch(d, context.Background(), parsed)
				if strings.TrimSpace(reply) != "" {
					injectAgentMessage(e, sessionID, reply)
				}
			}
		}

		injectPromptResponse(e, promptID)
		logging.Infof("chat: %s steward turn complete (dispatched=%v)", chatID, found)
	}()
}

// sendInject marshals a frame and sends it to the web via the inject
// channel. Shared by injectAgentMessage and injectPromptResponse.
func sendInject(e *chatEntry, frame any) {
	data, err := json.Marshal(frame)
	if err != nil {
		return
	}
	select {
	case e.inject <- data:
	case <-e.done:
	}
}

// injectAgentMessage constructs a session/update notification carrying an
// agent_message_chunk and sends it to the web via the inject channel.
func injectAgentMessage(e *chatEntry, sessionID, text string) {
	sendInject(e, map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": text,
				},
			},
		},
	})
}

// injectPromptResponse constructs a session/prompt response (the JSON-RPC
// response matching the original prompt request id) and sends it to the web.
// This resolves the web client's prompt() promise and clears the busy state.
func injectPromptResponse(e *chatEntry, promptID string) {
	if promptID == "" {
		return
	}
	sendInject(e, map[string]any{
		"jsonrpc": "2.0",
		"id":      promptID,
		"result": map[string]any{
			"stopReason": "end_turn",
		},
	})
}
