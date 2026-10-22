package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

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
