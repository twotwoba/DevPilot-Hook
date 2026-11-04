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
	// 输出带空白,顺带验证 TrimSpace;会话名 + 窗口号同取(v0.3.1)。
	// 每次调用追加一行到计数文件。
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ' main 2 '\necho x >> "+counter+"\n"), 0o755); err != nil {
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
	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest"}`), &stdout, &stderr)

	if gotSession != "main" {
		t.Fatalf("tmux session header = %q, want main", gotSession)
	}
	if gotWindow != "2" {
		t.Fatalf("tmux window header = %q, want 2", gotWindow)
	}
	// 探测次数 1~2:有控制 tty 祖先时先 list-panes 锚定,桩无 tab 输出
	// 不命中再回落 display-message;无 tty 祖先时只走 display-message。
	// 合同是「有限次数」,两环境都不得越界。
	if n := tmuxCalls(t, counter); n < 1 || n > 2 {
		t.Fatalf("tmux 探测次数 = %d, want 1~2", n)
	}
}

// TestParseTmuxInfo 解析规则:会话名可含空格——行尾最后一个空格分隔
// token 为窗口号(全数字校验),其余全量保留。
func TestParseTmuxInfo(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		session string
		window  string
	}{
		{"普通", "main 2", "main", "2"},
		{"会话名含空格", "my session 3", "my session", "3"},
		{"带首尾空白", "  dev session 12  ", "dev session", "12"},
		{"窗口非数字置空(余下段保留)", "main x", "main", ""},
		{"无 token 视作纯会话名", "solo", "solo", ""},
		{"空行", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTmuxInfo(tc.line)
			if got.session != tc.session || got.window != tc.window {
				t.Fatalf("parseTmuxInfo(%q) = {%q %q}, want {%q %q}",
					tc.line, got.session, got.window, tc.session, tc.window)
			}
		})
	}
}

// TestParsePaneList list-panes -a 输出按 pane tty 精确匹配:
// 会话名可含空格、行尾 token 为窗口号; tty 归一化(/dev/ 前缀、"?"/"??" 不算)。
func TestParsePaneList(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		tty     string
		session string
		window  string
	}{
		{"命中窗口0", "/dev/ttys019\tDevPilot\t0\ndev/null\n", "ttys019", "DevPilot", "0"},
		{"多行选对 pane", "/dev/ttys001\t0\t0\n/dev/ttys019\tmy work\t3\n/dev/ttys010\tDevPilot\t1\n", "ttys019", "my work", "3"},
		{"无 /dev 前缀也命中", "ttys011\tbug\t2\n", "ttys011", "bug", "2"},
		{"tty 不匹配落空", "/dev/ttys001\tmain\t5\n", "ttys999", "", ""},
		{"无 tty 行不算命中", "?\tmain\t5\n??\tmain\t6\n", "?", "", ""},
		{"窗口非数字置空会话保留", "/dev/ttys019\tmain\tx\n", "ttys019", "main", ""},
		{"缺字段行跳过", "onlytty\n", "onlytty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePaneList(tc.out, tc.tty)
			if got.session != tc.session || got.window != tc.window {
				t.Fatalf("parsePaneList(tty=%q) = {%q %q}, want {%q %q}",
					tc.tty, got.session, got.window, tc.session, tc.window)
			}
		})
	}
}

// TestHookTmuxPaneTTYPreferred 有控制 tty 的环境下,窗口号取 agent pane
// 真位置(list-panes 精确锚定),而非 display-message 回落的「会话当前
// 激活窗」。桩让两条路径输出不同窗口号,断言取的是 pane 那份。
func TestHookTmuxPaneTTYPreferred(t *testing.T) {
	tty := ancestorTTY()
	if tty == "" {
		t.Skip("测试进程自身无控制 tty(如 CI),tty 精确锚定无从验证")
	}

	bin := t.TempDir()
	script := filepath.Join(bin, "tmux")
	// list-panes:真 pane(ttys 命中本测试 tty,窗口 2)+ 两行 decoy;
	// display-message:激活窗回落,故意输出不同窗口号 9。
	stub := "#!/bin/sh\ncase \"$*\" in\n" +
		"*list-panes*)" +
		"printf '/dev/ttys001\\t0\\t0\\n/dev/" + tty + "\\tmain\\t2\\n/dev/ttys010\\tmain\\t9\\n';;" +
		"*)echo ' main 9 ';;\nesac\n"
	if err := os.WriteFile(script, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")

	var gotSession, gotWindow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-DevPilot-Tmux-Session")
		gotWindow = r.Header.Get("X-DevPilot-Tmux-Window")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	paths, _ := newTestPaths(t, portOf(t, srv.URL))
	var stdout, stderr bytes.Buffer
	runHook([]string{"--dir", paths.Dir, "claude"},
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`), &stdout, &stderr)

	if gotSession != "main" {
		t.Fatalf("tmux session header = %q, want main", gotSession)
	}
	if gotWindow != "2" {
		t.Fatalf("tmux window header = %q, want 2(pane 真位置,非激活窗 9)", gotWindow)
	}
}

// TestHookTmuxProbeEventWhitelist 探测名单外的事件不探测:
// 高频工具 hook 不能被 tmux 服务端卡死拖累;Stop(回合结束 relay)
// 低频且收件箱路由需要,在名单内。
func TestHookTmuxProbeEventWhitelist(t *testing.T) {
	cases := []struct {
		name      string
		eventName string
		mode      string // 空 = 默认 permission-request
		probe     bool
	}{
		{"permission_request 默认模式", "PermissionRequest", "", true},
		{"stop 回合结束 relay", "Stop", "", true},
		{"pre_tool_use 审批模式", "PreToolUse", string(config.ApprovalModePreToolUse), true},
		{"post_tool_use 不探测", "PostToolUse", "", false},
		{"pre_tool_use 非审批模式不探测", "PreToolUse", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := fakeTmux(t)
			var session, window string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				session = r.Header.Get("X-DevPilot-Tmux-Session")
				window = r.Header.Get("X-DevPilot-Tmux-Window")
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
				if session != "main" {
					t.Fatalf("session header = %q, want main", session)
				}
				if window != "2" {
					t.Fatalf("window header = %q, want 2", window)
				}
			} else {
				if session != "" || window != "" {
					t.Fatalf("不应带 tmux header,得到 session=%q window=%q", session, window)
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
