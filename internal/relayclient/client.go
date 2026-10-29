// Package relayclient 维护 daemon 到 relay 的 WSS 长连接。
package relayclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/event"
	"github.com/twotwoba/DevPilot-Hook/internal/id"
	"github.com/twotwoba/DevPilot-Hook/internal/queue"
)

// 心跳与退避默认值。
const (
	DefaultPingInterval = 30 * time.Second
	DefaultBackoffMin   = time.Second
	DefaultBackoffMax   = 30 * time.Second
	pingTimeout         = 10 * time.Second
	maxMessageBytes     = 1 << 20
)

// Dialer 便于测试注入假 relay。
type Dialer func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

// Config 是客户端配置。
type Config struct {
	RelayURL     string
	Credential   string
	DaemonID     string
	WAL          *queue.WAL
	Registry     *event.Registry
	Logger       *slog.Logger
	PingInterval time.Duration
	BackoffMin   time.Duration
	BackoffMax   time.Duration
	Dialer       Dialer
	Now          func() time.Time
}

// Status 是可观测状态。
type Status struct {
	Connected       bool   `json:"connected"`
	LastError       string `json:"last_error,omitempty"`
	LastConnectedAt int64  `json:"last_connected_at,omitempty"`
	LastAckAt       int64  `json:"last_ack_at,omitempty"`
	InFlight        int    `json:"in_flight"`
}

// Client 是单连接、自动重连的 WSS 客户端。
type Client struct {
	cfg Config
	log *slog.Logger

	wake chan struct{}

	writeMu  sync.Mutex // 同一时刻只允许一个写者
	mu       sync.Mutex
	status   Status
	inFlight map[string]bool
}

// New 校验配置并创建客户端。
func New(cfg Config) (*Client, error) {
	if cfg.RelayURL == "" || cfg.Credential == "" || cfg.DaemonID == "" {
		return nil, errors.New("relayclient: relay_url / credential / daemon_id must not be empty")
	}
	if cfg.WAL == nil || cfg.Registry == nil {
		return nil, errors.New("relayclient: missing WAL or Registry")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = DefaultBackoffMin
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = DefaultBackoffMax
	}
	if cfg.Dialer == nil {
		cfg.Dialer = websocket.Dial
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{
		cfg:      cfg,
		log:      cfg.Logger,
		wake:     make(chan struct{}, 1),
		inFlight: make(map[string]bool),
	}, nil
}

// Publish 唤醒发送循环；事件已由调用方写入 WAL。
func (c *Client) Publish(_ context.Context, _ wire.Envelope) error {
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

// Status 返回当前连接状态。
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.InFlight = len(c.inFlight)
	return s
}

// Run 阻塞运行重连循环，直到 ctx 取消。
func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.BackoffMin
	for {
		started := c.cfg.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		c.setDisconnected(err)

		// 连接稳定超过 30 秒才重置退避，避免「连上即断」时高频重试。
		if c.cfg.Now().Sub(started) > 30*time.Second {
			backoff = c.cfg.BackoffMin
		}
		wait := jitter(backoff)
		c.log.Warn("relay connection lost, reconnecting soon", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff *= 2
		if backoff > c.cfg.BackoffMax {
			backoff = c.cfg.BackoffMax
		}
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	target, err := wsURL(c.cfg.RelayURL)
	if err != nil {
		return err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := c.cfg.Dialer(dialCtx, target, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.cfg.Credential}},
		// 云端 Caddy 会做 TLS 终止与反向代理，默认 HTTP/1.1 Upgrade 即可。
	})
	cancelDial()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("relayclient: connect failed (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("relayclient: connect failed: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxMessageBytes)
	c.log.Info("connected to relay", "url", target)

	hello := wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: newMsgID(),
		Role: wire.RoleDaemon, DaemonID: c.cfg.DaemonID, TS: c.cfg.Now().Unix(),
	}
	if err := c.writeJSON(ctx, conn, hello); err != nil {
		return err
	}
	c.markConnected()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- c.readLoop(connCtx, conn) }()
	go func() { errCh <- c.writeLoop(connCtx, conn) }()

	err = <-errCh
	cancel()
	conn.CloseNow()
	<-errCh

	// 未确认的事件在重连后必须重发。
	c.mu.Lock()
	c.inFlight = make(map[string]bool)
	c.mu.Unlock()
	return err
}

// writeLoop 负责补发 WAL、转发新事件与应用层心跳。
func (c *Client) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	if err := c.flushPending(ctx, conn); err != nil {
		return err
	}
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.wake:
			if err := c.flushPending(ctx, conn); err != nil {
				return err
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			c.writeMu.Lock()
			err := conn.Ping(pingCtx)
			c.writeMu.Unlock()
			cancel()
			if err != nil {
				return fmt.Errorf("relayclient: ping failed: %w", err)
			}
		}
	}
}

// flushPending 按 WAL 顺序发送尚未在途的事件。
func (c *Client) flushPending(ctx context.Context, conn *websocket.Conn) error {
	for _, env := range c.cfg.WAL.Pending() {
		c.mu.Lock()
		if c.inFlight[env.EventID] {
			c.mu.Unlock()
			continue
		}
		c.inFlight[env.EventID] = true
		c.mu.Unlock()

		if err := c.writeJSON(ctx, conn, env); err != nil {
			c.mu.Lock()
			delete(c.inFlight, env.EventID)
			c.mu.Unlock()
			return err
		}
	}
	return nil
}

// readLoop 处理决策与 ack。
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		msg, err := wire.Decode(data)
		if err != nil {
			c.log.Warn("failed to decode relay message", "err", err)
			continue
		}

		switch m := msg.(type) {
		case *wire.Decision:
			c.handleDecision(ctx, conn, m)
		case *wire.Ack:
			c.handleAck(m)
		case *wire.HelloAck:
			c.log.Debug("relay acknowledged handshake")
		case *wire.Ping:
			_ = c.writeJSON(ctx, conn, wire.Pong{
				V: wire.Version, Kind: wire.KindPong, RefTime: m.TS, TS: c.cfg.Now().Unix(),
			})
		case *wire.Pong:
			// 心跳应答，无需处理。
		default:
			c.log.Debug("ignoring relay message", "kind", msgKind(msg))
		}
	}
}

func (c *Client) handleDecision(ctx context.Context, conn *websocket.Conn, d *wire.Decision) {
	res := c.cfg.Registry.Decide(d.ApprovalID, d.Decision, c.cfg.Now())
	ack := wire.Ack{
		V: wire.Version, Kind: wire.KindAck, MsgID: newMsgID(),
		RefMsgID: d.MsgID, ApprovalID: d.ApprovalID,
		Status: ackStatus(res.Status), TS: c.cfg.Now().Unix(),
	}
	if err := c.writeJSON(ctx, conn, ack); err != nil {
		c.log.Warn("failed to send decision ack", "approval_id", d.ApprovalID, "err", err)
	}
	c.log.Info("received decision from phone", "approval_id", d.ApprovalID, "decision", d.Decision, "status", res.Status)
}

func (c *Client) handleAck(a *wire.Ack) {
	if a.EventID == "" {
		return
	}
	if err := c.cfg.WAL.Ack(a.EventID); err != nil {
		c.log.Warn("failed to clean up WAL", "event_id", a.EventID, "err", err)
	}
	c.mu.Lock()
	delete(c.inFlight, a.EventID)
	c.status.LastAckAt = c.cfg.Now().Unix()
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := wire.Encode(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *Client) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Connected = true
	c.status.LastError = ""
	c.status.LastConnectedAt = c.cfg.Now().Unix()
}

func (c *Client) setDisconnected(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Connected = false
	if err != nil && !errors.Is(err, context.Canceled) {
		c.status.LastError = err.Error()
	}
}

func ackStatus(s event.Status) wire.AckStatus {
	switch s {
	case event.StatusApproved, event.StatusDenied:
		return wire.AckAccepted
	case event.StatusExpired:
		return wire.AckExpired
	case event.StatusUnknown:
		return wire.AckUnknown
	default:
		return wire.AckRejected
	}
}

func msgKind(v any) string {
	switch v.(type) {
	case *wire.ResyncResult:
		return string(wire.KindResyncResult)
	case *wire.LATokenAck:
		return string(wire.KindLATokenAck)
	default:
		return "unknown"
	}
}

// wsURL 把 https://host 转换为 wss://host/ws/daemon。
func wsURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("relayclient: invalid relay_url: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("relayclient: unsupported relay_url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("relayclient: relay_url missing host")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/ws/daemon") {
		path += "/ws/daemon"
	}
	u.Path = path
	return u.String(), nil
}

// jitter 在 [0.8, 1.2) 倍之间抖动，避免多台机器同时重连。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	factor := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * factor)
}

func newMsgID() string { return id.New() }
