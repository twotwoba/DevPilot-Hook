package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/agent/claude"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
	"github.com/twotwoba/DevPilot-Hook/internal/event"
	"github.com/twotwoba/DevPilot-Hook/internal/queue"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakePublisher struct {
	mu     sync.Mutex
	events []wire.Envelope
	ch     chan wire.Envelope
}

func (f *fakePublisher) Publish(_ context.Context, env wire.Envelope) error {
	f.mu.Lock()
	f.events = append(f.events, env)
	f.mu.Unlock()
	select {
	case f.ch <- env:
	default:
	}
	return nil
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// eventsOfType 返回已上行事件中指定类型的快照。
func (f *fakePublisher) eventsOfType(typ wire.EventType) []wire.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []wire.Envelope
	for _, env := range f.events {
		if env.Type == typ {
			out = append(out, env)
		}
	}
	return out
}

type testEnv struct {
	server  *Server
	http    *httptest.Server
	pub     *fakePublisher
	wal     *queue.WAL
	reg     *event.Registry
	timeout time.Duration
}

func newTestEnv(t *testing.T, timeout time.Duration, interval time.Duration) *testEnv {
	return newTestEnvOpts(t, timeout, interval, nil)
}

// newTestEnvOpts 允许覆盖额外 Params(HostInfo / Now 等新依赖)。
func newTestEnvOpts(t *testing.T, timeout time.Duration, interval time.Duration, extra func(*Params)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	wal, err := queue.Open(filepath.Join(dir, "queue.wal"), queue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })

	pub := &fakePublisher{ch: make(chan wire.Envelope, 32)}
	reg := event.NewRegistry()
	p := Params{
		Addr:              "127.0.0.1:0",
		Token:             testToken,
		DaemonID:          "dd_test",
		ApprovalTimeout:   timeout,
		ToolEventInterval: interval,
		ApprovalMode:      config.ApprovalModePermissionRequest,
		Adapters:          map[wire.Agent]agent.Adapter{wire.AgentClaude: claude.Adapter{}},
		Registry:          reg,
		WAL:               wal,
		Publisher:         pub,
	}
	if extra != nil {
		extra(&p)
	}
	srv, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{server: srv, http: ts, pub: pub, wal: wal, reg: reg, timeout: timeout}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hooks", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type reqOpts struct {
	token  string
	origin string
	host   string
	agent  string
	body   []byte
	method string
	path   string
	noBody bool
	tmux   string
}

func (e *testEnv) do(t *testing.T, o reqOpts) (*http.Response, []byte) {
	t.Helper()
	if o.method == "" {
		o.method = http.MethodPost
	}
	if o.path == "" {
		o.path = "/v1/hook/" + o.agent
	}
	var body io.Reader
	if !o.noBody {
		body = bytes.NewReader(o.body)
	}
	req, err := http.NewRequest(o.method, e.http.URL+o.path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = o.host
	if req.Host == "" {
		req.Host = "127.0.0.1:24543"
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	if o.origin != "" {
		req.Header.Set("Origin", o.origin)
	}
	if o.tmux != "" {
		req.Header.Set("X-DevPilot-Tmux-Session", o.tmux)
	}
	resp, err := e.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func TestAuthAndOrigin(t *testing.T) {
	e := newTestEnv(t, 2*time.Second, 0)

	resp, _ := e.do(t, reqOpts{path: "/v1/status", method: http.MethodGet, noBody: true})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，实际 %d", resp.StatusCode)
	}

	resp, _ = e.do(t, reqOpts{path: "/v1/status", method: http.MethodGet, noBody: true, token: "wrong"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 token 应 401，实际 %d", resp.StatusCode)
	}

	resp, _ = e.do(t, reqOpts{path: "/v1/status", method: http.MethodGet, noBody: true, token: testToken, origin: "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("带 Origin 应 403，实际 %d", resp.StatusCode)
	}

	resp, _ = e.do(t, reqOpts{path: "/v1/status", method: http.MethodGet, noBody: true, token: testToken, host: "evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("非回环 Host 应 403，实际 %d", resp.StatusCode)
	}

	resp, data := e.do(t, reqOpts{path: "/v1/status", method: http.MethodGet, noBody: true, token: testToken})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法请求失败: %d %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "dd_test") {
		t.Fatalf("状态内容不符: %s", data)
	}
}

func TestNonApprovalEventEnqueued(t *testing.T) {
	e := newTestEnv(t, 2*time.Second, 0)
	resp, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "pre_tool_use.json")})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("期望 204，实际 %d %s", resp.StatusCode, body)
	}
	if e.wal.Len() != 1 || e.pub.count() != 1 {
		t.Fatalf("事件应入队并上行: wal=%d pub=%d", e.wal.Len(), e.pub.count())
	}
	got := e.wal.Pending()[0]
	if got.Type != wire.EventToolRunning || got.Payload.CommandSummary != "npm test" {
		t.Fatalf("事件内容不符: %+v", got)
	}
}

func TestApprovalAllowRoundTrip(t *testing.T) {
	e := newTestEnv(t, 3*time.Second, 0)

	type result struct {
		status int
		body   []byte
	}
	done := make(chan result, 1)
	go func() {
		resp, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "permission_request.json")})
		done <- result{resp.StatusCode, body}
	}()

	env := waitEvent(t, e.pub.ch)
	if env.Type != wire.EventApprovalRequired || env.ApprovalID == "" {
		t.Fatalf("审批事件不符: %+v", env)
	}

	resp, body := e.do(t, reqOpts{
		path: "/v1/decisions", token: testToken,
		body: mustJSON(t, wire.Decision{
			V: wire.Version, Kind: wire.KindDecision, MsgID: "m1",
			ApprovalID: env.ApprovalID, Decision: wire.DecisionAllow, TS: time.Now().Unix(),
		}),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("决策提交失败: %d %s", resp.StatusCode, body)
	}

	r := <-done
	if r.status != http.StatusOK {
		t.Fatalf("hook 期望 200，实际 %d", r.status)
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
			Decision      struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("hook 输出不是 JSON: %s", r.body)
	}
	if out.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("期望 allow: %s", r.body)
	}
}

func TestApprovalDenyAndDuplicate(t *testing.T) {
	e := newTestEnv(t, 3*time.Second, 0)

	done := make(chan []byte, 1)
	go func() {
		_, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "permission_request.json")})
		done <- body
	}()
	env := waitEvent(t, e.pub.ch)

	decision := func(d wire.DecisionKind) map[string]any {
		resp, body := e.do(t, reqOpts{
			path: "/v1/decisions", token: testToken,
			body: mustJSON(t, wire.Decision{
				V: wire.Version, Kind: wire.KindDecision, MsgID: string(d),
				ApprovalID: env.ApprovalID, Decision: d, TS: time.Now().Unix(),
			}),
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("决策提交失败: %d", resp.StatusCode)
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		return m
	}

	first := decision(wire.DecisionDeny)
	if first["accepted"] != true {
		t.Fatalf("首次决策应生效: %v", first)
	}
	second := decision(wire.DecisionAllow)
	if second["accepted"] != false || second["status"] != "denied" {
		t.Fatalf("重复决策必须 first-wins: %v", second)
	}

	body := <-done
	if !strings.Contains(string(body), `"behavior":"deny"`) {
		t.Fatalf("hook 应得到 deny: %s", body)
	}
}

func TestApprovalTimeoutReturnsEmpty(t *testing.T) {
	e := newTestEnv(t, 50*time.Millisecond, 0)

	start := time.Now()
	resp, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "permission_request.json")})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("超时应 204，实际 %d %s", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Fatalf("超时不得输出决策: %s", body)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时生效过慢: %v", elapsed)
	}
}

func TestToolEventsThrottled(t *testing.T) {
	e := newTestEnv(t, 2*time.Second, time.Hour)
	body := fixture(t, "pre_tool_use.json")

	for range 2 {
		resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken, body: body})
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("期望 204，实际 %d", resp.StatusCode)
		}
	}
	if e.pub.count() != 1 || e.wal.Len() != 1 {
		t.Fatalf("窗口内应只发一条: pub=%d wal=%d", e.pub.count(), e.wal.Len())
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	e := newTestEnv(t, time.Second, 0)
	big := bytes.Repeat([]byte("a"), MaxBodyBytes+10)
	resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken, body: big})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求应 413，实际 %d", resp.StatusCode)
	}
}

func TestUnknownAgent(t *testing.T) {
	e := newTestEnv(t, time.Second, 0)
	resp, _ := e.do(t, reqOpts{agent: "other", token: testToken, body: []byte(`{}`)})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知 agent 应 404，实际 %d", resp.StatusCode)
	}
}

func waitEvent(t *testing.T, ch <-chan wire.Envelope) wire.Envelope {
	t.Helper()
	select {
	case env := <-ch:
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("等待事件超时")
		return wire.Envelope{}
	}
}

// waitClose 等待 close 事件;普通事件先到则跳过(顺序不保证)。
func waitClose(t *testing.T, ch <-chan wire.Envelope) wire.Envelope {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-ch:
			if env.Type == wire.EventApprovalClosed {
				return env
			}
		case <-deadline:
			t.Fatal("等待 approval_closed 超时")
			return wire.Envelope{}
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestHostContextStamped 验证主机身份/会话上下文/expires_at 盖章:
// approval_required 盖全量字段,普通工具事件只盖身份、不盖 expires_at/tmux。
func TestHostContextStamped(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	e := newTestEnvOpts(t, 2*time.Minute, 0, func(p *Params) {
		p.Now = func() time.Time { return fixed }
		p.HostInfo = func() agent.HostInfo {
			return agent.HostInfo{Hostname: "eric-mbp.lan", PrimaryIP: "192.168.1.23", User: "eric"}
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, body := e.do(t, reqOpts{
			agent: "claude", token: testToken, tmux: "main",
			body: fixture(t, "permission_request.json"),
		})
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("hook 状态 = %d %s", resp.StatusCode, body)
		}
	}()

	env := waitEvent(t, e.pub.ch)
	if env.Type != wire.EventApprovalRequired {
		t.Fatalf("事件类型 = %s", env.Type)
	}
	if env.Payload.Hostname != "eric-mbp.lan" || env.Payload.HostIP != "192.168.1.23" || env.Payload.User != "eric" {
		t.Fatalf("主机身份缺失: %+v", env.Payload)
	}
	if env.Payload.Cwd != "/Users/eric/Dev/demo-app" {
		t.Fatalf("cwd = %q", env.Payload.Cwd)
	}
	if env.Payload.TmuxSession != "main" {
		t.Fatalf("tmux 会话 = %q", env.Payload.TmuxSession)
	}
	if env.ExpiresAt != fixed.Add(2*time.Minute).Unix() {
		t.Fatalf("expires_at = %d, want %d", env.ExpiresAt, fixed.Add(2*time.Minute).Unix())
	}
	<-done

	// 工具事件:身份照盖,但 expires_at 与 tmux 不适用。
	resp, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "pre_tool_use.json")})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("工具事件状态 = %d %s", resp.StatusCode, body)
	}
	tool := e.wal.Pending()[e.wal.Len()-1]
	if tool.Type != wire.EventToolRunning {
		t.Fatalf("工具事件类型 = %s", tool.Type)
	}
	if tool.Payload.Hostname != "eric-mbp.lan" || tool.Payload.Cwd == "" {
		t.Fatalf("工具事件应带主机身份: %+v", tool.Payload)
	}
	if tool.ExpiresAt != 0 || tool.Payload.TmuxSession != "" {
		t.Fatalf("工具事件不应盖 expires_at/tmux: %+v", tool)
	}
}

// TestApprovalTimeoutEmitsClose 超时路径:hook 拿空 204 的同时,
// close(expired) 经 WAL/上行发出,EventID 确定性派生。
func TestApprovalTimeoutEmitsClose(t *testing.T) {
	e := newTestEnv(t, 50*time.Millisecond, 0)

	resp, body := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "permission_request.json")})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("超时应 204，实际 %d %s", resp.StatusCode, body)
	}

	closed := waitClose(t, e.pub.ch)
	if closed.EventID != "close-"+closed.ApprovalID {
		t.Fatalf("close EventID = %q (approval=%s)", closed.EventID, closed.ApprovalID)
	}
	if closed.Payload.StatusText != wire.CloseReasonExpired {
		t.Fatalf("close 原因 = %q", closed.Payload.StatusText)
	}
	if closed.ExpiresAt != 0 {
		t.Fatalf("close 不应带 expires_at: %d", closed.ExpiresAt)
	}
	if closed.MsgID == "" || closed.Type != wire.EventApprovalClosed {
		t.Fatalf("close 信封不完整: %+v", closed)
	}
}

// TestApprovalCancelEmitsClose 终端 Ctrl-C 路径:hook 客户端断开
// → ctx 取消 → cancelled close 仍要上行(独立 context)。
func TestApprovalCancelEmitsClose(t *testing.T) {
	e := newTestEnv(t, time.Minute, 0)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.http.URL+"/v1/hook/claude", bytes.NewReader(fixture(t, "permission_request.json")))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:24543"
	req.Header.Set("Authorization", "Bearer "+testToken)
	go func() { _, _ = e.http.Client().Do(req) }()

	env := waitEvent(t, e.pub.ch)
	cancel()

	closed := waitClose(t, e.pub.ch)
	if closed.EventID != "close-"+env.ApprovalID {
		t.Fatalf("close EventID = %q (approval=%s)", closed.EventID, env.ApprovalID)
	}
	if closed.Payload.StatusText != wire.CloseReasonCancelled {
		t.Fatalf("close 原因 = %q", closed.Payload.StatusText)
	}
}

// TestApprovalDecisionEmitsNoClose 决策路径不发自产 close:
// relay 落地决策时会合成,再发就是重复(靠下游去重兜底不如源头不发)。
func TestApprovalDecisionEmitsNoClose(t *testing.T) {
	e := newTestEnv(t, 3*time.Second, 0)

	done := make(chan int, 1)
	go func() {
		resp, _ := e.do(t, reqOpts{agent: "claude", token: testToken, body: fixture(t, "permission_request.json")})
		done <- resp.StatusCode
	}()
	env := waitEvent(t, e.pub.ch)

	resp, body := e.do(t, reqOpts{
		path: "/v1/decisions", token: testToken,
		body: mustJSON(t, wire.Decision{
			V: wire.Version, Kind: wire.KindDecision, MsgID: "m1",
			ApprovalID: env.ApprovalID, Decision: wire.DecisionAllow, TS: time.Now().Unix(),
		}),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("决策提交失败: %d %s", resp.StatusCode, body)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("hook 期望 200，实际 %d", code)
	}
	// Wait 已返回、响应已发完,事件列表定格:只有 approval_required。
	if got := e.pub.eventsOfType(wire.EventApprovalClosed); len(got) != 0 {
		t.Fatalf("决策路径不应发 close: %+v", got)
	}
}
