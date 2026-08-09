package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/skip2/go-qrcode"

	"github.com/eushing/agentwork/internal/events"
)

// Connector is the IM connection manager: the Web-driven Feishu connect
// flow. The owner clicks "连接飞书" in the Web UI → a QR code is issued
// (SDK one-click app registration) → the owner scans it in Feishu → the app
// credentials are created → the long connection starts → the owner sends one
// message to the bot → the receive target is captured → CONNECTED. All state
// persists to app_settings, so the daemon auto-reconnects on startup with no
// environment configuration (DESIGN.v2.md decision 2-14; the product
// interaction, not env vars).
type ConnStatus string

const (
	StatusIdle           ConnStatus = "idle"
	StatusWaitingQR      ConnStatus = "waiting_qr"       // QR issued, awaiting scan
	StatusWaitingMessage ConnStatus = "waiting_message"  // app created, awaiting first message to capture the receive target
	StatusConnected      ConnStatus = "connected"
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

	store   SettingsStore
	bus     *events.Bus
	client  *larkClientHolder // the live SDK client + ws (built on connect)
	notify  *Notifier         // milestone pusher, armed once connected
	stop    context.CancelFunc
}

// larkClientHolder keeps the long-connection resources for the current
// config (rebuilt on reconnect).
type larkClientHolder struct {
	ws *larkws.Client
}

// NewConnector creates the connection manager. store persists the Feishu
// config; bus is where the milestone pusher subscribes once connected.
func NewConnector(store SettingsStore, bus *events.Bus) *Connector {
	return &Connector{store: store, bus: bus, status: StatusIdle}
}

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
				Name: "agentwork",
				Desc: "agentwork AI 工作流（自动化闭环）",
			},
			Addons: &registration.AppAddons{
				Scopes: registration.AppAddonsScopes{
					Tenant: []string{"im:message", "im:message:send_as_bot"},
				},
				Events: registration.AppAddonsEvents{
					Items: registration.AppAddonsEventItems{
						Tenant: []string{"im.message.receive_v1"},
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
		c.connectWithCurrent(regCtx)
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

// connectWithCurrent starts the long connection for the current config. The
// first inbound user message captures the receive target (when unset) and
// flips the status to connected.
func (c *Connector) connectWithCurrent(ctx context.Context) error {
	c.mu.Lock()
	appID, appSecret := c.config.AppID, c.config.AppSecret
	receive := c.config.ReceiveID
	c.mu.Unlock()

	dh := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, event *larkim.P2MessageReceiveV1) error {
			c.captureReceive(event)
			return nil
		})
	ws := larkws.NewClient(appID, appSecret, larkws.WithEventHandler(dh))

	c.mu.Lock()
	if c.stop != nil {
		c.stop()
	}
	wsCtx, stop := context.WithCancel(ctx)
	c.stop = stop
	c.mu.Unlock()

	// The notifier shares the same credentials for outbound pushes and
	// subscribes the milestone events once armed.
	n := New(appID, appSecret, c.receiveTypeLocked(), receive)
	n.client = larkClientFor(appID, appSecret)
	if c.bus != nil {
		n.Subscribe(c.bus)
	}
	c.mu.Lock()
	c.notify = n
	c.mu.Unlock()

	if receive != "" {
		c.mu.Lock()
		c.status = StatusConnected
		c.mu.Unlock()
	}
	log.Printf("notify: feishu long connection up (app %s)", appID)
	return ws.Start(wsCtx)
}

// captureReceive records the first inbound user message's target as the
// push destination and persists it.
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

	if !already && receiveID != "" {
		raw, _ := json.Marshal(cfg)
		if err := c.store.Set(context.Background(), settingsKey, string(raw)); err != nil {
			log.Printf("notify: persist feishu config: %v", err)
		}
		log.Printf("notify: receive target captured (%s %s) — connected", receiveType, receiveID)
	}
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
