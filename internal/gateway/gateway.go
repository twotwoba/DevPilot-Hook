// Package gateway 实现 127.0.0.1 上的本地 hook 网关。
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
	"github.com/twotwoba/DevPilot-Hook/internal/event"
	"github.com/twotwoba/DevPilot-Hook/internal/queue"
)

// MaxBodyBytes 是单个 hook 请求体上限。
const MaxBodyBytes = 1 << 20

// Publisher 把事件交给上行通道；必须快速返回，真正重试由 WAL 保证。
type Publisher interface {
	Publish(ctx context.Context, env wire.Envelope) error
}

// Params 是网关依赖。
type Params struct {
	Addr              string
	Token             string
	DaemonID          string
	ApprovalTimeout   time.Duration
	ToolEventInterval time.Duration
	ApprovalMode      config.ApprovalMode
	Adapters          map[wire.Agent]agent.Adapter
	Registry          *event.Registry
	WAL               *queue.WAL
	Publisher         Publisher
	Now               func() time.Time
	Logger            *slog.Logger
	// ExtraStatus 供 doctor 汇总上行通道状态。
	ExtraStatus func() map[string]any
}

// Server 是本地网关。
type Server struct {
	p        Params
	throttle *event.Throttle
	log      *slog.Logger

	mu   sync.Mutex
	addr string
}

// New 校验依赖并创建网关。
func New(p Params) (*Server, error) {
	if p.Token == "" {
		return nil, errors.New("gateway: 缺少本地 token")
	}
	if p.DaemonID == "" {
		return nil, errors.New("gateway: 缺少 daemon_id")
	}
	if p.Registry == nil || p.WAL == nil {
		return nil, errors.New("gateway: 缺少 Registry 或 WAL")
	}
	if len(p.Adapters) == 0 {
		return nil, errors.New("gateway: 未注册任何 agent 适配器")
	}
	if p.ApprovalTimeout <= 0 {
		p.ApprovalTimeout = wire.ApprovalWaitSeconds * time.Second
	}
	if p.ApprovalMode == "" {
		p.ApprovalMode = config.ApprovalModePermissionRequest
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return &Server{
		p:        p,
		throttle: event.NewThrottle(p.ToolEventInterval),
		log:      p.Logger,
		addr:     p.Addr,
	}, nil
}

// Addr 返回实际监听地址（监听前返回配置值）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Handler 返回 HTTP 处理器（测试可直接挂到 httptest）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.localOnly(s.handleHealth))
	mux.HandleFunc("GET /v1/status", s.guard(s.handleStatus))
	mux.HandleFunc("POST /v1/hook/{agent}", s.guard(s.handleHook))
	mux.HandleFunc("POST /v1/events", s.guard(s.handleEvent))
	mux.HandleFunc("POST /v1/decisions", s.guard(s.handleDecision))
	return mux
}

// ListenAndServe 阻塞运行，直到 ctx 取消。
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.p.Addr)
	if err != nil {
		return fmt.Errorf("gateway: 监听失败: %w", err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	s.log.Info("本地网关已启动", "addr", s.addr)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localOnly 只允许本机回环访问，并拒绝浏览器发起的跨站请求。
func (s *Server) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "不允许浏览器来源的请求")
			return
		}
		if !validHost(r.Host) {
			writeError(w, http.StatusForbidden, "Host 必须是本机回环地址")
			return
		}
		next(w, r)
	}
}

// guard 在 localOnly 之上校验 Bearer token。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return s.localOnly(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(got) != len(s.p.Token) ||
			subtle.ConstantTimeCompare([]byte(got), []byte(s.p.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "本地 token 无效")
			return
		}
		next(w, r)
	})
}

func validHost(host string) bool {
	h := host
	if name, _, err := net.SplitHostPort(host); err == nil {
		h = name
	}
	switch h {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{
		"daemon_id":         s.p.DaemonID,
		"gateway_addr":      s.Addr(),
		"pending_approvals": s.p.Registry.Len(),
		"queue_depth":       s.p.WAL.Len(),
	}
	if s.p.ExtraStatus != nil {
		maps.Copy(resp, s.p.ExtraStatus())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := wire.Agent(r.PathValue("agent"))
	adapter, ok := s.p.Adapters[name]
	if !ok {
		writeError(w, http.StatusNotFound, "不支持的 agent: "+string(name))
		return
	}

	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}
	in, err := agent.ParseInput(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	now := s.p.Now()
	ev, err := adapter.Normalize(in, s.p.DaemonID, now.Unix(), s.p.ApprovalMode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ev == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	env := ev.Envelope

	// 高频工具遥测做固定窗口节流；审批、任务完成、会话边界永不丢弃。
	if env.Type == wire.EventToolRunning || env.Type == wire.EventToolFinished {
		if !s.throttle.Allow(env.SessionID+":"+string(env.Type), now) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if env.Type == wire.EventSessionEnded {
		s.throttle.Forget(env.SessionID + ":")
	}

	if !ev.NeedsApproval {
		if err := s.enqueue(r.Context(), env); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 审批先登记再入队：重复 hook 不会产生第二条审批。
	_, isNew := s.p.Registry.Add(env, now, s.p.ApprovalTimeout)
	if isNew {
		if err := s.enqueue(r.Context(), env); err != nil {
			s.p.Registry.Cancel(env.ApprovalID)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	result := s.p.Registry.Wait(r.Context(), env.ApprovalID)
	switch result.Status {
	case event.StatusApproved, event.StatusDenied:
		out, err := adapter.RenderDecision(ev.HookEvent, result.Decision)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(out, '\n'))
	default:
		// 超时 / 取消 / 未知：不输出任何 decision，让 Agent 回落原生询问。
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}
	msg, err := wire.Decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	env, ok := msg.(*wire.Envelope)
	if !ok {
		writeError(w, http.StatusBadRequest, "只接受 kind=event")
		return
	}
	if env.DaemonID != s.p.DaemonID {
		writeError(w, http.StatusBadRequest, "daemon_id 不匹配")
		return
	}
	if err := s.enqueue(r.Context(), *env); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(w, r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}
	var d wire.Decision
	if err := json.Unmarshal(body, &d); err != nil {
		writeError(w, http.StatusBadRequest, "决策 JSON 无效")
		return
	}
	if err := d.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res := s.p.Registry.Decide(d.ApprovalID, d.Decision, s.p.Now())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   res.Status,
		"decision": res.Decision,
		"accepted": res.Accepted,
	})
}

// enqueue 先落 WAL 再上行；上行失败不影响 hook 正确性，重连后补发。
func (s *Server) enqueue(ctx context.Context, env wire.Envelope) error {
	if err := s.p.WAL.Put(env); err != nil {
		return err
	}
	if s.p.Publisher != nil {
		if err := s.p.Publisher.Publish(ctx, env); err != nil {
			s.log.Debug("事件暂存 WAL，等待重连补发", "event_id", env.EventID, "err", err)
		}
	}
	return nil
}

func readLimited(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	reader := http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	data, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, maxErr
		}
		return nil, err
	}
	return data, nil
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
