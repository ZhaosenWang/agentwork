package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/skip2/go-qrcode"

	"github.com/eushing/agentwork/internal/events"
	"github.com/eushing/agentwork/internal/service"
)

// Connector is the IM connection manager: the Web-driven Feishu connect
// flow. The owner clicks "连接飞书" in the Web UI → a QR code is issued
// (SDK one-click app registration) → the owner scans it in Feishu → the app
// credentials are created → the long connection starts → the owner sends one
// message to the bot → the receive target is captured → CONNECTED. All state
// persists to app_settings, so the daemon auto-reconnects on startup with no
// environment configuration (DESIGN.md decision 2-14; the product
// interaction, not env vars).
type ConnStatus string

const (
	StatusIdle           ConnStatus = "idle"
	StatusWaitingQR      ConnStatus = "waiting_qr"       // QR issued, awaiting scan
	StatusWaitingMessage ConnStatus = "waiting_message"  // app created, awaiting first message to capture the receive target
	StatusConnected      ConnStatus = "connected"
	StatusReconnecting   ConnStatus = "reconnecting"     // connection dropped; the reconnect loop is backing off (M4)
	StatusFailed         ConnStatus = "failed"
)

// QRInfo is the registration QR the frontend renders (base64 PNG + URL).
type QRInfo struct {
	URL       string `json:"url"`
	ImgBase64 string `json:"img_base64"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds
}

// FeishuConfig is the persisted connection state (app_settings key
// "im.feishu").
type FeishuConfig struct {
	AppID        string `json:"app_id"`
	AppSecret    string `json:"app_secret"`
	ReceiveID    string `json:"receive_id"`
	ReceiveType  string `json:"receive_type"` // chat_id | open_id
	// OwnerOpenID is the scanned app's owner (the person who authorized the
	// registration QR) — the platform's single user. It is the authority for
	// M3 inbound commands and approval-card callbacks. ReceiveID is the PUSH
	// target (often a chat_id of the p2p/group conversation the owner uses
	// with the bot) and must NOT be conflated with the owner: a p2p chat has
	// a chat_id too, so sender(open_id) == ReceiveID never holds once the
	// capture stored the chat.
	OwnerOpenID string `json:"owner_open_id"`
}

// SettingsStore abstracts the persistence (the daemon wires the SQLite
// settings service; tests inject a map).
type SettingsStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

const settingsKey = "im.feishu"

type Connector struct {
	mu       sync.Mutex
	status   ConnStatus
	lastErr  string
	qr       QRInfo
	connectID string
	config   FeishuConfig

	store     SettingsStore
	bus       *events.Bus
	qs        QueryStore         // M3: card evidence + digest queries (may be nil)
	goalSvc   *service.GoalService // M3: approval-card callbacks resolve here
	intakeSvc *IntakeService       // M3: inbound message pipeline (may be nil)
	notify    *Notifier            // milestone pusher, armed once connected
	stop      context.CancelFunc
}

// NewConnector creates the connection manager. store persists the Feishu
// config; bus is where the milestone pusher subscribes once connected.
func NewConnector(store SettingsStore, bus *events.Bus) *Connector {
	return &Connector{store: store, bus: bus, status: StatusIdle}
}

// SetQueryStore wires the read-only store (M3) so the notifier built on
// connect can carry card evidence and digest data.
func (c *Connector) SetQueryStore(qs QueryStore) { c.qs = qs }

// SetGoalService wires the approval-callback resolver (M3). The card buttons
// call back over the long connection; the decisions go through the goal
// layer's ResolveReview — the same arbitration as the Web approval panel.
func (c *Connector) SetGoalService(gs *service.GoalService) { c.goalSvc = gs }

// SetIntakeService wires the inbound pipeline (M3). The owner's messages
// become parse runs on the configured global parser agent.
func (c *Connector) SetIntakeService(s *IntakeService) { c.intakeSvc = s }

// ── State ──

// Status returns a snapshot for the Web UI.
func (c *Connector) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"status":      string(c.status),
		"receive_id":  c.config.ReceiveID,
		"app_id":      c.config.AppID,
		"error":       c.lastErr,
		"qr":          c.qr,
		"connect_id":  c.connectID,
	}
}

// ── Registration flow (Web-driven) ──

// StartRegistration issues the QR code for the SDK one-click app
// registration. The flow runs in a goroutine (blocking until scan or the
// 10-minute timeout); the frontend polls Status() while it completes.
func (c *Connector) StartRegistration(ctx context.Context) (string, QRInfo, error) {
	c.mu.Lock()
	if c.status == StatusConnected {
		c.mu.Unlock()
		return "", QRInfo{}, fmt.Errorf("already connected")
	}
	if c.status == StatusWaitingQR {
		c.mu.Unlock()
		return c.connectID, c.qr, nil
	}
	c.status = StatusWaitingQR
	c.lastErr = ""
	c.connectID = uuid.NewString()
	c.mu.Unlock()

	regCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	go func() {
		defer cancel()
		result, err := registration.RegisterApp(regCtx, &registration.Options{
			AppPreset: &registration.AppPreset{
				Name: "agentwork-bot",
				Desc: "agentwork butler: your AI workflow steward — work goes to me, checkpoints come to you",
			},
			Addons: &registration.AppAddons{
				Scopes: registration.AppAddonsScopes{
					Tenant: []string{"im:message", "im:message:send_as_bot"},
				},
				Events: registration.AppAddonsEvents{
					Items: registration.AppAddonsEventItems{
						// im.message.receive_v1: inbound messages (M1 receive
						// target capture; M3 inbound task creation).
						// card.action.trigger: approval-card button callbacks
						// (M3) — the long connection is the event channel, no
						// public callback URL.
						Tenant: []string{"im.message.receive_v1", "card.action.trigger"},
					},
				},
			},
			OnQRCode: func(info *registration.QRCodeInfo) {
				c.cacheQR(info)
			},
		})
		if err != nil {
			c.mu.Lock()
			c.status = StatusFailed
			c.lastErr = "registration: " + err.Error()
			c.mu.Unlock()
			log.Printf("notify: feishu registration failed: %v", err)
			return
		}
		c.mu.Lock()
		c.config.AppID = result.ClientID
		c.config.AppSecret = result.ClientSecret
		c.status = StatusWaitingMessage
		c.mu.Unlock()
		// Resolve the push target from the AUTHORIZATION itself: the app's
		// owner is whoever scanned the QR, and GetApplication returns their
		// open_id. No "send a message" step — scanning IS the connection.
		// (If the owner lookup fails, fall back to waiting_message: the first
		// bot message then captures the target instead.)
		ownerOpenID, ownerErr := resolveOwnerOpenID(result.ClientID, result.ClientSecret)
		c.mu.Lock()
		if ownerErr == nil {
			// The owner is the platform's single user (M3 authority). The
			// receive target can still be re-captured as a chat by the first
			// inbound message — the owner identity is kept separate.
			c.config.OwnerOpenID = ownerOpenID
			c.config.ReceiveID = ownerOpenID
			c.config.ReceiveType = "open_id"
			c.status = StatusConnected
		} else {
			c.status = StatusWaitingMessage
		}
		cfg := c.config
		c.mu.Unlock()
		// Persist the credentials IMMEDIATELY — a daemon restart before the
		// receive target is known must not lose the app (previously
		// credentials only landed on captureReceive).
		if raw, err := json.Marshal(cfg); err == nil {
			if err := c.store.Set(context.Background(), settingsKey, string(raw)); err != nil {
				log.Printf("notify: persist feishu app credentials: %v", err)
			}
		}
		if ownerErr != nil {
			log.Printf("notify: feishu app created (%s), owner lookup failed (%v) — waiting for first message", result.ClientID, ownerErr)
		} else {
			log.Printf("notify: feishu connected to app owner %s", ownerOpenID)
		}
		// The long connection must NOT ride on regCtx: that context carries a
		// 10-minute registration timeout, and a cancelled context kills the
		// WS (and its auto-reconnect) 10 minutes after the scan — the
		// connection silently died and the UI kept showing "connected"
		// (regression found in M4). A background context keeps the
		// connection alive; Disconnect stops it via c.stop.
		c.connectWithCurrent(context.Background())
	}()
	return c.connectID, c.qr, nil
}

func (c *Connector) cacheQR(info *registration.QRCodeInfo) {
	png, err := qrcode.Encode(info.URL, qrcode.Medium, 256)
	if err != nil {
		log.Printf("notify: encode QR: %v", err)
		return
	}
	c.mu.Lock()
	c.qr = QRInfo{
		URL:       info.URL,
		ImgBase64: base64.StdEncoding.EncodeToString(png),
		ExpiresAt: time.Now().Unix() + int64(info.ExpireIn),
	}
	c.mu.Unlock()
}

// ── Connection ──

// Start resumes a persisted connection (daemon startup): if the config
// exists in settings, connect immediately. Blocks while the long connection
// is alive.
func (c *Connector) Start(ctx context.Context) error {
	if raw, err := c.store.Get(ctx, settingsKey); err == nil && raw != "" {
		var cfg FeishuConfig
		if json.Unmarshal([]byte(raw), &cfg) == nil && cfg.AppID != "" && cfg.AppSecret != "" {
			c.mu.Lock()
			c.config = cfg
			c.mu.Unlock()
			if cfg.ReceiveID != "" {
				c.mu.Lock()
				c.status = StatusConnected
				c.mu.Unlock()
			}
			log.Printf("notify: feishu config found, connecting (app %s, receive %s)", cfg.AppID, cfg.ReceiveID)
			return c.connectWithCurrent(ctx)
		}
	}
	log.Println("notify: no feishu config — connect via the Web UI (Settings → 连接飞书)")
	<-ctx.Done()
	return nil
}

// connectWithCurrent runs the long connection for the current config, with a
// reconnect loop: the SDK's own auto-reconnect covers transient drops, and
// when ws.Start finally returns (credentials rejected, SDK gave up) this loop
// backs off and reconnects — otherwise a drop after the SDK gave up would end
// the goroutine and the connection would never come back (the M4 regression:
// the UI kept showing "connected" while inbound messages silently died).
// The notifier is created ONCE (the reconnect loop must not re-subscribe the
// bus — every event would then be pushed N times).
func (c *Connector) connectWithCurrent(ctx context.Context) error {
	c.mu.Lock()
	appID, appSecret := c.config.AppID, c.config.AppSecret
	receive := c.config.ReceiveID
	first := c.notify == nil
	c.mu.Unlock()

	if first {
		// The notifier shares the same credentials for outbound pushes and
		// subscribes the milestone events once armed. Its QueryStore (card
		// evidence / digest data) comes from the connector's wiring.
		n := New(appID, appSecret, c.receiveTypeLocked(), receive)
		n.client = larkClientFor(appID, appSecret)
		n.qs = c.qs
		if c.bus != nil {
			n.Subscribe(c.bus)
		}
		c.mu.Lock()
		c.notify = n
		c.mu.Unlock()
	}

	dh := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
			c.captureReceive(event)
			return nil
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return c.onCardAction(ctx, event)
		})

	// A connection that drops in seconds, repeatedly, is not a network blip —
	// it is almost always dead credentials (the app was deleted, the secret
	// rotated). Retrying forever would hammer the Feishu API in a loop; after
	// maxReconnectFailures the connector gives up, reports failed, and waits
	// for the owner to reconnect (重新扫码). Each SUCCESSFUL long connection
	// resets the counter (the loop only exits on deliberate stop).
	const maxReconnectFailures = 3
	backoff := 5 * time.Second
	failures := 0
	for {
		ws := larkws.NewClient(appID, appSecret,
			larkws.WithEventHandler(dh),
			larkws.WithOnReconnecting(func() { c.setStatus(StatusReconnecting) }),
			larkws.WithOnReconnected(func() {
				c.mu.Lock()
				c.status = StatusConnected
				c.mu.Unlock()
			}),
		)
		wsCtx, stop := context.WithCancel(ctx)
		c.mu.Lock()
		if c.stop != nil {
			c.stop() // stop the previous loop's connection if any
		}
		c.stop = stop
		if c.config.ReceiveID != "" {
			c.status = StatusConnected
		}
		c.mu.Unlock()

		err := ws.Start(wsCtx)
		if ctx.Err() != nil || wsCtx.Err() != nil {
			return err // deliberate stop (Disconnect / shutdown)
		}
		failures++
		if failures >= maxReconnectFailures {
			c.mu.Lock()
			c.lastErr = "connection keeps dropping — check the Feishu app credentials and reconnect"
			c.status = StatusFailed
			c.mu.Unlock()
			log.Printf("notify: feishu connection failed %d times in a row — giving up, waiting for reconnect", failures)
			return nil
		}
		log.Printf("notify: feishu long connection dropped (%v) — reconnecting in %s (%d/%d)", err, backoff, failures, maxReconnectFailures)
		c.setStatus(StatusReconnecting)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// setStatus updates the connection state seen by the Web UI.
func (c *Connector) setStatus(s ConnStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

// ownerOpenID resolves the platform's single user: the app owner recorded at
// registration, falling back to the open_id receive target (configs created
// before OwnerOpenID existed). An empty result means "no owner known yet"
// (pre-scan or a chat-only capture) — inbound commands then no-op.
func (c *Connector) ownerOpenID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config.OwnerOpenID != "" {
		return c.config.OwnerOpenID
	}
	if c.config.ReceiveType == "open_id" {
		return c.config.ReceiveID
	}
	return ""
}

// ── Approval-card callbacks (M3-1) ──

// onCardAction resolves an approval-card button decision (card.action.trigger
// over the long connection). The buttons carry {action, goal_id, run_id} in
// the value; the optional reject-reason input arrives in form_value. The
// decision goes through the goal layer's ResolveReview — the same
// arbitration as the Web panel — so the IM path cannot bypass the state
// machine. The original card is then updated to its processed state and the
// button clicker gets a toast.
func (c *Connector) onCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil || event.Event.Action.Value == nil {
		return nil, nil
	}
	act := event.Event.Action
	goalID, _ := act.Value["goal_id"].(string)
	runID, _ := act.Value["run_id"].(string)
	decision, _ := act.Value["action"].(string)
	if goalID == "" || (decision != "approve" && decision != "reject") {
		return nil, nil
	}
	// Single-user guard: only the owner (the scanned app's owner) may
	// decide. The card is delivered to the owner only, so this is a cheap
	// defense-in-depth, not the authorization model.
	op := ""
	if event.Event.Operator != nil {
		op = event.Event.Operator.OpenID
	}
	owner := c.ownerOpenID()
	if owner != "" && op != "" && op != owner {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "warning", Content: "无权操作",
		}}, nil
	}
	if c.goalSvc == nil {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "error", Content: "平台未就绪（goalSvc 未接线）",
		}}, nil
	}
	// The reject-reason input's value arrives in form_value (the input's name
	// is reject_reason, set in buildReviewCard).
	reason := ""
	if r, ok := act.FormValue["reject_reason"]; ok {
		if s, ok := r.(string); ok {
			reason = s
		}
	}
	if _, err := c.goalSvc.ResolveReview(ctx, goalID, runID, decision, reason); err != nil {
		// The validator's message is developer-oriented; the toast must be
		// human-oriented (the most common case: a duplicate click while the
		// async deliver runs — the card still shows buttons until the update
		// lands, so the human clicks again).
		msg := "操作失败，请稍后重试"
		if strings.Contains(err.Error(), "already approved") || strings.Contains(err.Error(), "being delivered") {
			msg = "该任务已批准，正在合入中——请稍候，无需重复操作"
		} else if strings.Contains(err.Error(), "not in review") {
			msg = "该任务已不在待审批状态（可能已被处理）"
		}
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "error", Content: msg,
		}}, nil
	}
	// Stamp the original card as processed (best-effort; the decision is
	// already recorded).
	if event.Event.Context != nil && event.Event.Context.OpenMessageID != "" {
		go c.updateCardProcessed(event.Event.Context.OpenMessageID, goalID, decision)
	}
	toast := "已批准，平台开始合入"
	if decision == "reject" {
		toast = "已驳回，goal 将带决策意见重跑"
	}
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
		Type: "success", Content: toast,
	}}, nil
}

// updateCardProcessed replaces the approval card with its processed state via
// the IM message update API (PATCH /im/v1/messages/:message_id).
func (c *Connector) updateCardProcessed(messageID, goalID, decision string) {
	c.mu.Lock()
	n := c.notify
	c.mu.Unlock()
	if n == nil || n.client == nil {
		return
	}
	content, err := buildProcessedCard(goalID, decision)
	if err != nil {
		return
	}
	// The message-update API (PATCH /im/v1/messages/:message_id) takes ONLY
	// content — a message's type is immutable and msg_type in the body is
	// rejected (code 230001, "invalid msg_type"; regression found the first
	// time a Feishu approval card was actually updated).
	resp, err := n.client.Im.Message.Update(context.Background(),
		larkim.NewUpdateMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewUpdateMessageReqBodyBuilder().
				Content(content).
				Build()).
			Build())
	if err != nil {
		log.Printf("notify: update approval card %s: %v", messageID, err)
		return
	}
	if !resp.Success() {
		log.Printf("notify: update approval card %s failed: code=%d msg=%s", messageID, resp.Code, resp.Msg)
	}
}

// captureReceive records the first inbound user message's target as the
// push destination and persists it. From M3 it also dispatches the owner's
// inbound messages to the intake pipeline (natural-language task creation
// via a processor run).
func (c *Connector) captureReceive(event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	// Ignore the bot's own echoes (sender is the bot app).
	sender := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil && event.Event.Sender.SenderId.OpenId != nil {
		sender = *event.Event.Sender.SenderId.OpenId
	}
	c.mu.Lock()
	if sender != "" && sender == c.config.AppID {
		c.mu.Unlock()
		return
	}
	receiveType, receiveID := "chat_id", ""
	if msg.ChatId != nil {
		receiveID = *msg.ChatId
	} else if event.Event.Sender != nil && event.Event.Sender.SenderId != nil && event.Event.Sender.SenderId.OpenId != nil {
		receiveType, receiveID = "open_id", *event.Event.Sender.SenderId.OpenId
	}
	already := c.config.ReceiveID != ""
	c.config.ReceiveID = receiveID
	c.config.ReceiveType = receiveType
	// The milestone pusher may have been created BEFORE the receive target
	// existed (the scan flow connects in waiting_message state) — update its
	// target in place, otherwise every push fails with "no receive target".
	if c.notify != nil {
		c.notify.receiveID = receiveID
		c.notify.receiveIDType = receiveType
	}
	cfg := c.config
	if !already {
		c.status = StatusConnected
	}
	c.mu.Unlock()

	// Persist whenever a target is captured/re-captured: the scan flow may
	// already have set ReceiveID (the owner's open_id) — the chat target the
	// user actually messages from must survive a daemon restart.
	if receiveID != "" {
		raw, _ := json.Marshal(cfg)
		if err := c.store.Set(context.Background(), settingsKey, string(raw)); err != nil {
			log.Printf("notify: persist feishu config: %v", err)
		}
		if !already {
			log.Printf("notify: receive target captured (%s %s) — connected", receiveType, receiveID)
		}
	}

	c.dispatchInbound(event, sender)
}

// ── Inbound messages (M3-4) ──

// dispatchInbound feeds the owner's messages to the intake pipeline: an
// immediate "received" acknowledgement, then a processor run that parses the
// natural-language request into an action (intake.json, file-as-side-effect)
// which the daemon executes and replies to. Only the owner (the receive
// target — whoever scanned the registration QR) may command; group messages
// must @ the bot, p2p chat needs no mention.
func (c *Connector) dispatchInbound(event *larkim.P2MessageReceiveV1, sender string) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	if msg.MessageType == nil || *msg.MessageType != "text" {
		return
	}
	// The message text lives in a JSON content envelope {"text":"..."}.
	var env struct{ Text string `json:"text"` }
	if msg.Content == nil || json.Unmarshal([]byte(*msg.Content), &env) != nil || strings.TrimSpace(env.Text) == "" {
		return
	}
	// The owner is the scanned app's owner (open_id), NOT the push target:
	// ReceiveID is often a chat_id (p2p chats have chat ids too), so comparing
	// the sender's open_id to it would never match.
	owner := c.ownerOpenID()
	if owner == "" || sender == "" || sender != owner {
		return
	}
	// Group chat: require the bot mention. p2p: the owner's message IS the
	// command channel (no mention exists in p2p with a bot).
	isP2P := msg.ChatType != nil && *msg.ChatType == "p2p"
	if !isP2P && !containsMention(msg) {
		return
	}
	// Immediate acknowledgement — the parse run may queue behind a busy
	// worker agent.
	if n := c.notifier(); n != nil {
		n.asyncSend("⏳ 收到，正在解析你的消息…")
	}
	if c.intakeSvc == nil {
		if n := c.notifier(); n != nil {
			n.asyncSend("⚠️ 平台未就绪（intake 未接线），请稍后再试")
		}
		return
	}
	prompt, err := c.intakeSvc.BuildPrompt(context.Background(), env.Text)
	if err != nil {
		if n := c.notifier(); n != nil {
			n.asyncSend("⚠️ " + err.Error())
		}
		return
	}
	msgID := ""
	if msg.MessageId != nil {
		msgID = *msg.MessageId
	}
	if _, err := c.intakeSvc.Enqueue(context.Background(), msgID, prompt); err != nil {
		if n := c.notifier(); n != nil {
			n.asyncSend("⚠️ 解析任务创建失败：" + err.Error())
		}
	}
}

// containsMention reports whether the message @s our bot (mentioned_type=bot).
func containsMention(msg *larkim.EventMessage) bool {
	if msg.Mentions == nil {
		return false
	}
	for _, m := range msg.Mentions {
		if m != nil && m.MentionedType != nil && *m.MentionedType == "bot" {
			return true
		}
	}
	return false
}

func (c *Connector) notifier() *Notifier {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notify
}

func (c *Connector) receiveTypeLocked() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config.ReceiveType == "" {
		return "chat_id"
	}
	return c.config.ReceiveType
}

// Disconnect clears the stored config and stops the long connection.
func (c *Connector) Disconnect(ctx context.Context) error {
	c.mu.Lock()
	if c.stop != nil {
		c.stop()
	}
	c.status = StatusIdle
	c.config = FeishuConfig{}
	c.notify = nil
	c.mu.Unlock()
	return c.store.Delete(ctx, settingsKey)
}

// Notifier returns the armed milestone pusher (nil until connected).
func (c *Connector) Notifier() *Notifier {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notify
}

// larkClientFor builds the SDK client for outbound IM calls (shared
// credentials with the long connection).
func larkClientFor(appID, appSecret string) *lark.Client {
	return lark.NewClient(appID, appSecret)
}

// resolveOwnerOpenID finds the push target from the authorization itself:
// the app's owner is whoever scanned the registration QR, and
// GetApplication (app_id=me) returns their open_id (user_id_type=open_id).
// This removes the "send a message to the bot" step — scanning IS the
// connection.
func resolveOwnerOpenID(appID, appSecret string) (string, error) {
	client := lark.NewClient(appID, appSecret)
	resp, err := client.Application.Application.Get(context.Background(),
		larkapplication.NewGetApplicationReqBuilder().
			AppId("me").
			Lang("zh_cn").
			UserIdType("open_id").
			Build())
	if err != nil {
		return "", fmt.Errorf("get application: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("get application failed: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.App == nil || resp.Data.App.Owner == nil ||
		resp.Data.App.Owner.OwnerId == nil || *resp.Data.App.Owner.OwnerId == "" {
		return "", fmt.Errorf("application owner has no open_id")
	}
	return *resp.Data.App.Owner.OwnerId, nil
}
