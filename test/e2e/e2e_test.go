//go:build e2e

// Package e2e 用真实二进制跑通「hook → daemon → relay → 假 App → 决策回流」。
//
// 运行方式（需要 DevPilot-APNS 源码同级或设置 DEVPILOT_APNS_DIR）：
//
//	go test -tags=e2e ./test/e2e -v
//
// 测试会现场编译 devpilot-hook 与 relay，不依赖预装产物。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// approvalTimeout 是测试里的审批等待窗口；足够覆盖一次人工决策。
const approvalTimeout = 4 * time.Second

// harness 保存一次端到端环境的所有手柄。
type harness struct {
	t             *testing.T
	hookBin       string
	hookDir       string
	relayURL      string
	gatewayURL    string
	appCredential string
	token         string
	app           *websocket.Conn
	deviceID      string

	relayLog *lockedBuffer
	serveLog *lockedBuffer
}

// lockedBuffer 让命令输出可以被测试与 exec goroutine 安全并发访问。
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestApprovalRoundTrip(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	t.Run("allow", func(t *testing.T) {
		h.runApproval(t, "toolu_e2e_allow", wire.DecisionAllow, `"behavior":"allow"`)
	})
	t.Run("deny", func(t *testing.T) {
		h.runApproval(t, "toolu_e2e_deny", wire.DecisionDeny, `"behavior":"deny"`)
	})
	t.Run("timeout 无输出", func(t *testing.T) {
		h.runTimeout(t, "toolu_e2e_timeout")
	})
}

// newHarness 编译两个二进制、启动 relay、完成配对并拉起 daemon。
func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("缺少 go 命令: %v", err)
	}
	hookRoot := moduleRoot(t)
	apnsRoot := os.Getenv("DEVPILOT_APNS_DIR")
	if apnsRoot == "" {
		apnsRoot = filepath.Join(filepath.Dir(hookRoot), "DevPilot-APNS")
	}
	if _, err := os.Stat(filepath.Join(apnsRoot, "go.mod")); err != nil {
		t.Skipf("未找到 DevPilot-APNS 源码（可用 DEVPILOT_APNS_DIR 指定）: %v", err)
	}

	tmp := t.TempDir()
	h := &harness{
		t:        t,
		hookBin:  filepath.Join(tmp, "devpilot-hook"),
		relayLog: &lockedBuffer{},
		serveLog: &lockedBuffer{},
	}
	buildBin(t, hookRoot, h.hookBin, "./cmd/devpilot-hook")
	relayBin := filepath.Join(tmp, "devpilot-relay")
	buildBin(t, apnsRoot, relayBin, "./cmd/relay")

	// relay 使用随机端口与临时数据目录；APNs 走 Mock，避免依赖真机凭据。
	relayPort := freePort(t)
	h.relayURL = fmt.Sprintf("http://127.0.0.1:%d", relayPort)
	relayCmd := exec.Command(relayBin, "serve")
	relayCmd.Env = append(os.Environ(),
		fmt.Sprintf("LISTEN_ADDR=127.0.0.1:%d", relayPort),
		"DATA_DIR="+filepath.Join(tmp, "relay-data"),
		"APNS_MOCK=1",
		"LOG_LEVEL=error",
	)
	relayCmd.Stdout = h.relayLog
	relayCmd.Stderr = h.relayLog
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("启动 relay 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = relayCmd.Process.Kill()
		_, _ = relayCmd.Process.Wait()
	})
	waitHTTP(t, h.relayURL+"/healthz", 20*time.Second, h.relayLog)

	// 用真实 pair 命令注册配对会话，再以假 App 身份完成配对。
	h.hookDir = filepath.Join(tmp, "hook")
	code := h.pair(h.hookDir)
	h.pairAsApp(code)
	h.configure(h.hookDir)

	// 拉起 daemon（serve 同时负责网关与 WSS 上行）。
	serveCmd := exec.Command(h.hookBin, "serve", "--dir", h.hookDir, "--foreground")
	serveCmd.Stdout = h.serveLog
	serveCmd.Stderr = h.serveLog
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("启动 devpilot-hook serve 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = serveCmd.Process.Kill()
		_, _ = serveCmd.Process.Wait()
	})

	cfg, err := config.Load(config.NewPaths(h.hookDir))
	if err != nil {
		t.Fatalf("读取 daemon 配置失败: %v", err)
	}
	h.gatewayURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.GatewayPort)
	waitHTTP(t, h.gatewayURL+"/healthz", 20*time.Second, h.serveLog)
	h.token = readToken(t, h.hookDir)
	h.waitDaemonConnected()

	// 假 App 连上 relay，后续所有决策都从这条连接下发。
	h.app = h.dialApp()
	return h
}

func (h *harness) close() {
	if h.app != nil {
		_ = h.app.Close(websocket.StatusNormalClosure, "")
	}
}

// pair 运行真实 pair 命令并提取配对码。
func (h *harness) pair(hookDir string) string {
	h.t.Helper()
	out, err := exec.Command(h.hookBin, "pair", "--dir", hookDir, "--relay", h.relayURL).CombinedOutput()
	if err != nil {
		h.t.Fatalf("pair 失败: %v\n%s", err, out)
	}
	re := regexp.MustCompile(`^[A-Z2-7]{16,}$`)
	for _, field := range strings.Fields(string(out)) {
		if re.MatchString(field) {
			return field
		}
	}
	h.t.Fatalf("pair 输出中未找到配对码:\n%s", out)
	return ""
}

// pairAsApp 模拟 iOS App 用配对码换取设备凭据。
func (h *harness) pairAsApp(code string) string {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"code": code, "apns_token": "e2e-apns-token", "device_name": "E2E iPhone",
	})
	resp, err := http.Post(h.relayURL+"/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("配对请求失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("配对失败 HTTP %d: %s", resp.StatusCode, data)
	}
	var out struct {
		DeviceID         string `json:"device_id"`
		DeviceCredential string `json:"device_credential"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		h.t.Fatalf("解析配对响应失败: %v", err)
	}
	h.deviceID = out.DeviceID
	// 设备凭据只在内存中使用，不落盘。
	h.appCredential = out.DeviceCredential
	return out.DeviceID
}

// configure 调整 daemon 配置：固定网关端口并缩短审批等待。
func (h *harness) configure(hookDir string) {
	h.t.Helper()
	paths := config.NewPaths(hookDir)
	cfg, err := config.Load(paths)
	if err != nil {
		h.t.Fatalf("读取配置失败: %v", err)
	}
	cfg.GatewayPort = freePort(h.t)
	cfg.ApprovalTimeout = int(approvalTimeout.Seconds())
	if err := cfg.Save(paths); err != nil {
		h.t.Fatalf("写入配置失败: %v", err)
	}
}

func (h *harness) dialApp() *websocket.Conn {
	h.t.Helper()
	wsURL := strings.Replace(h.relayURL, "http://", "ws://", 1) + "/ws/app"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + h.appCredential}},
	})
	if err != nil {
		h.t.Fatalf("App WSS 连接失败: %v", err)
	}
	if err := writeJSON(ctx, conn, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_e2e_app",
		Role: wire.RoleApp, DeviceID: h.deviceID, TS: time.Now().Unix(),
	}); err != nil {
		h.t.Fatalf("发送 App hello 失败: %v", err)
	}
	msg := readMsg(h.t, conn, 10*time.Second)
	if _, ok := msg.(*wire.HelloAck); !ok {
		h.t.Fatalf("期望 hello_ack，收到 %T", msg)
	}
	return conn
}

// runApproval 跑一次 hook 子进程，并在假 App 侧下发指定决策。
func (h *harness) runApproval(t *testing.T, toolUseID string, decision wire.DecisionKind, wantOut string) {
	t.Helper()
	cmd, stdout, stderr := h.startHook(t, toolUseID)
	env := h.waitApproval(t, toolUseID)
	h.sendDecision(t, env.ApprovalID, decision)
	h.waitHookExit(t, cmd, 20*time.Second)

	// 重复决策必须 first-wins：第二次只返回 already_decided，不改变结果。
	h.sendDecisionExpect(t, env.ApprovalID, decision, wire.AckAlreadyDecided)

	got := stdout.String()
	if !strings.Contains(got, wantOut) {
		t.Fatalf("hook 输出缺少 %s，实际: %s（stderr: %s）", wantOut, got, stderr.String())
	}
	if !strings.Contains(got, `"hookEventName":"PermissionRequest"`) {
		t.Fatalf("hook 输出事件名不匹配: %s", got)
	}
	h.waitQueueDrained(t)
}

// runTimeout 验证超时只会产生「无决策」。
func (h *harness) runTimeout(t *testing.T, toolUseID string) {
	t.Helper()
	cmd, stdout, stderr := h.startHook(t, toolUseID)
	_ = h.waitApproval(t, toolUseID)
	// 不下发任何决策，等待网关按 ApprovalTimeout 放弃。
	h.waitHookExit(t, cmd, 20*time.Second)
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("超时必须无输出（绝不超时放行），实际: %q", got)
	}
	if strings.TrimSpace(stderr.String()) != "" {
		t.Fatalf("hook 不应向 stderr 输出: %s", stderr.String())
	}
}

func (h *harness) startHook(t *testing.T, toolUseID string) (*exec.Cmd, *lockedBuffer, *lockedBuffer) {
	t.Helper()
	payload := fmt.Sprintf(`{
		"session_id": "sess_e2e_%s",
		"transcript_path": "/tmp/sess_e2e.jsonl",
		"cwd": "/Users/e2e/demo",
		"hook_event_name": "PermissionRequest",
		"tool_name": "Bash",
		"tool_input": {"command": "printf e2e-%s"},
		"tool_use_id": %q
	}`, toolUseID, toolUseID, toolUseID)

	// 注意：flag 包在遇到第一个位置参数后停止解析，--dir 必须放在 agent 之前。
	cmd := exec.Command(h.hookBin, "hook", "--dir", h.hookDir, "claude")
	cmd.Stdin = strings.NewReader(payload)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 hook 子进程失败: %v", err)
	}
	return cmd, stdout, stderr
}

// waitApproval 从 App 连接读取直到拿到本次审批事件。
func (h *harness) waitApproval(t *testing.T, toolUseID string) *wire.Envelope {
	t.Helper()
	wantSession := "sess_e2e_" + toolUseID
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		msg := readMsg(t, h.app, time.Until(deadline))
		env, ok := msg.(*wire.Envelope)
		if !ok || env.Type != wire.EventApprovalRequired {
			continue
		}
		if env.ApprovalID == "" || env.SessionID != wantSession {
			continue
		}
		return env
	}
	t.Fatalf("未收到审批事件 %s\nrelay 日志:\n%s\ndaemon 日志:\n%s",
		toolUseID, h.relayLog.String(), h.serveLog.String())
	return nil
}

func (h *harness) sendDecision(t *testing.T, approvalID string, decision wire.DecisionKind) {
	t.Helper()
	h.sendDecisionExpect(t, approvalID, decision, wire.AckAccepted)
}

// sendDecisionExpect 下发决策并校验 relay 返回的状态。
func (h *harness) sendDecisionExpect(t *testing.T, approvalID string, decision wire.DecisionKind, want wire.AckStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgID := fmt.Sprintf("m_e2e_%s_%d", approvalID, time.Now().UnixNano())
	if err := writeJSON(ctx, h.app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: msgID,
		ApprovalID: approvalID, Decision: decision, TS: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("发送决策失败: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		msg := readMsg(t, h.app, time.Until(deadline))
		ack, ok := msg.(*wire.Ack)
		if !ok || ack.ApprovalID != approvalID || ack.RefMsgID != msgID {
			continue
		}
		if ack.Status != want {
			t.Fatalf("决策 ack 状态 = %s，期望 %s", ack.Status, want)
		}
		return
	}
	t.Fatalf("未收到决策 ack %s", approvalID)
}

func (h *harness) waitHookExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook 子进程异常退出: %v", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("hook 子进程未在 %s 内退出\ndaemon 日志:\n%s", timeout, h.serveLog.String())
	}
}

// waitQueueDrained 确认事件已被 relay ack、WAL 清空。
func (h *harness) waitQueueDrained(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		depth, err := h.queueDepth()
		if err == nil && depth == 0 {
			return
		}
		last = fmt.Sprintf("depth=%d err=%v", depth, err)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WAL 未清空: %s", last)
}

func (h *harness) queueDepth() (int, error) {
	req, err := http.NewRequest(http.MethodGet, h.gatewayURL+"/v1/status", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st struct {
		QueueDepth float64 `json:"queue_depth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return 0, err
	}
	return int(st.QueueDepth), nil
}

// waitDaemonConnected 轮询 relay 指标，确保 WSS 上行已就绪。
func (h *harness) waitDaemonConnected() {
	h.t.Helper()
	waitFor(h.t, 20*time.Second, "daemon 连接 relay", func() error {
		resp, err := http.Get(h.relayURL + "/metrics")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "devpilot_daemon_connections 1") {
			return nil
		}
		return fmt.Errorf("指标中尚无在线 daemon")
	})
}

// ---------- 基础设施 ----------

func buildBin(t *testing.T, dir, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 %s 失败: %v\n%s", pkg, err, data)
	}
}

// moduleRoot 从当前测试目录向上寻找 go.mod。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("分配端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitHTTP(t *testing.T, url string, timeout time.Duration, diag *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时: %v\n进程输出:\n%s", url, last, diag.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := fn(); err == nil {
			return
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时: %v", what, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readToken(t *testing.T, hookDir string) string {
	t.Helper()
	data, err := os.ReadFile(config.NewPaths(hookDir).TokenPath())
	if err != nil {
		t.Fatalf("读取网关 token 失败: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func readMsg(t *testing.T, conn *websocket.Conn, timeout time.Duration) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取 WSS 消息失败: %v", err)
	}
	msg, err := wire.Decode(data)
	if err != nil {
		t.Fatalf("解析 WSS 消息失败: %v\n%s", err, data)
	}
	return msg
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
