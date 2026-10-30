package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// newTestPaths 构造带配置与 token 的临时状态目录。
func newTestPaths(t *testing.T, port int) (config.Paths, string) {
	t.Helper()
	paths := config.NewPaths(t.TempDir())
	cfg := config.Default()
	cfg.DaemonID = "dd_test"
	cfg.GatewayPort = port
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	if err := os.WriteFile(paths.TokenPath(), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths, token
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHookRelaysDecision(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest"}}`))
	}))
	defer srv.Close()

	paths, token := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{"session_id":"s1"}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d，want 0", code)
	}
	if gotPath != "/v1/hook/claude" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if gotBody != `{"session_id":"s1"}` {
		t.Fatalf("请求体 = %q", gotBody)
	}
	if !strings.Contains(stdout.String(), "hookSpecificOutput") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHookSilentOnNoDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte(`{"kind":"decision"}`))
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("204 不应输出任何内容，得到 %q", stdout.String())
	}
}

func TestHookSilentWhenDaemonDown(t *testing.T) {
	// 先占一个端口再释放，使请求必然失败。
	srv := httptest.NewServer(http.NotFoundHandler())
	port := portOf(t, srv.URL)
	srv.Close()

	paths, _ := newTestPaths(t, port)
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(`{}`), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("daemon 不在时不应输出，得到 %q", stdout.String())
	}
}

func TestHookIgnoresUnknownAgent(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	if code := runHook([]string{"--dir", paths.Dir, "other"}, strings.NewReader(`{}`), &stdout, &stderr); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if hits != 0 {
		t.Fatalf("未知 agent 不应请求网关，hits = %d", hits)
	}
}

func TestHookMissingConfigSilent(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runHook([]string{"--dir", dir, "codex"}, strings.NewReader(`{}`), &stdout, &stderr)
	if code != 0 || stdout.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

func TestDoctorJSON(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			statusHits++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"queue_depth": 3, "pending_approvals": 1,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	code := runDoctor([]string{"--dir", paths.Dir, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr=%s", code, stderr.String())
	}
	if statusHits != 1 {
		t.Fatalf("status 命中 %d 次", statusHits)
	}

	var report struct {
		OK     bool          `json:"ok"`
		Checks []checkResult `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("解析 JSON 失败: %v\n%s", err, stdout.String())
	}
	if !report.OK {
		t.Fatalf("期望 ok=true: %s", stdout.String())
	}
	byName := map[string]checkResult{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if got := byName["gateway"]; got.Status != "ok" || !strings.Contains(got.Detail, "queue_depth=3") {
		t.Fatalf("gateway 检查 = %+v", got)
	}
	if got := byName["pairing"]; got.Status != "warn" {
		t.Fatalf("pairing 检查 = %+v", got)
	}
	// 未设置 RelayURL 时不应发起网络请求。
	if got := byName["relay"]; got.Status != "warn" {
		t.Fatalf("relay 检查 = %+v", got)
	}
}

func TestDoctorCorruptConfigFails(t *testing.T) {
	dir := t.TempDir()
	paths := config.NewPaths(dir)
	if err := os.WriteFile(paths.ConfigPath(), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--dir", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("退出码 = %d，want 1", code)
	}
	if !strings.Contains(stdout.String(), "config") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// TestHookClientTimeoutDerivation 锁住超时派生关系:
// 客户端超时(+50)必须严格小于 install 写入的 hook timeout(+60),
// daemon 的空 204 才能先于 Agent 侧超时到达。
func TestHookClientTimeoutDerivation(t *testing.T) {
	cfg := config.Default()
	if cfg.ApprovalTimeout != config.DefaultApprovalTimeoutSeconds {
		t.Fatalf("默认等待窗 = %d, want %d", cfg.ApprovalTimeout, config.DefaultApprovalTimeoutSeconds)
	}
	if d := hookClientTimeout(cfg); d != time.Duration(cfg.ApprovalTimeout+50)*time.Second {
		t.Fatalf("默认派生 = %v", d)
	}

	cfg.ApprovalTimeout = 60 // 用户自定义短窗同样满足约束
	d := hookClientTimeout(cfg)
	installTimeout := time.Duration(cfg.ApprovalTimeout+60) * time.Second
	if d >= installTimeout {
		t.Fatalf("客户端超时 %v 必须小于 install timeout %v", d, installTimeout)
	}
}

// fakeTmux 在 PATH 前插一个记录调用次数的 tmux 桩,返回计数文件路径。
// 同时放宽探测超时:整套 -race 并行时 fork/exec 可能被 CPU 饿超过生产 2s 兜底。
func fakeTmux(t *testing.T) string {
	t.Helper()
	oldTimeout := tmuxProbeTimeout
	tmuxProbeTimeout = 10 * time.Second
	t.Cleanup(func() { tmuxProbeTimeout = oldTimeout })

	bin := t.TempDir()
	counter := filepath.Join(bin, "calls")
	script := filepath.Join(bin, "tmux")
	// 输出带空白,顺带验证 TrimSpace;每次调用追加一行到计数文件。
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ' main '\necho x >> "+counter+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	return counter
}

func tmuxCalls(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(bytes.Split(data, []byte("\n"))) - 1
}

func TestHookTmuxHeaderInjected(t *testing.T) {
	counter := fakeTmux(t)
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-DevPilot-Tmux-Session")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest"}`), &stdout, &stderr)

	if gotHeader != "main" {
		t.Fatalf("tmux header = %q, want main", gotHeader)
	}
	if n := tmuxCalls(t, counter); n != 1 {
		t.Fatalf("tmux 探测次数 = %d, want 1", n)
	}
}

// TestHookTmuxProbeOnlyForApprovalCandidates 非审批事件不探测:
// 高频工具 hook 不能被 tmux 服务端卡死拖累。
func TestHookTmuxProbeOnlyForApprovalCandidates(t *testing.T) {
	cases := []struct {
		name      string
		eventName string
		mode      string // 空 = 默认 permission-request
		probe     bool
	}{
		{"permission_request 默认模式", "PermissionRequest", "", true},
		{"pre_tool_use 审批模式", "PreToolUse", string(config.ApprovalModePreToolUse), true},
		{"post_tool_use 不探测", "PostToolUse", "", false},
		{"pre_tool_use 非审批模式不探测", "PreToolUse", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := fakeTmux(t)
			var header string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				header = r.Header.Get("X-DevPilot-Tmux-Session")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			paths, _ := newTestPaths(t, portOf(t, srv.URL))
			if tc.mode != "" {
				cfg := config.Default()
				cfg.GatewayPort = portOf(t, srv.URL)
				cfg.ApprovalMode = tc.mode
				if err := cfg.Save(paths); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			body := `{"session_id":"s1","hook_event_name":"` + tc.eventName + `"}`
			runHook([]string{"--dir", paths.Dir, "claude"}, strings.NewReader(body), &stdout, &stderr)

			if tc.probe {
				if header != "main" {
					t.Fatalf("header = %q, want main", header)
				}
			} else {
				if header != "" {
					t.Fatalf("不应带 tmux header,得到 %q", header)
				}
				if n := tmuxCalls(t, counter); n != 0 {
					t.Fatalf("不应探测 tmux,实际 %d 次", n)
				}
			}
		})
	}
}

// TestHookTmuxEnvMissingSkipsProbe 无 $TMUX(非 tmux 环境)直接跳过。
func TestHookTmuxEnvMissingSkipsProbe(t *testing.T) {
	counter := fakeTmux(t)
	t.Setenv("TMUX", "")
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-DevPilot-Tmux-Session")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest"}`), &stdout, &stderr)

	if header != "" {
		t.Fatalf("无 TMUX 不应带 header,得到 %q", header)
	}
	if n := tmuxCalls(t, counter); n != 0 {
		t.Fatalf("无 TMUX 不应探测,实际 %d 次", n)
	}
}
