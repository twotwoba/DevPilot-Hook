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

type testEnv struct {
	server  *Server
	http    *httptest.Server
	pub     *fakePublisher
	wal     *queue.WAL
	reg     *event.Registry
	timeout time.Duration
}

func newTestEnv(t *testing.T, timeout time.Duration, interval time.Duration) *testEnv {
	t.Helper()
	dir := t.TempDir()
	wal, err := queue.Open(filepath.Join(dir, "queue.wal"), queue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close() })

	pub := &fakePublisher{ch: make(chan wire.Envelope, 32)}
	reg := event.NewRegistry()
	srv, err := New(Params{
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
	})
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
