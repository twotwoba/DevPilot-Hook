package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// readHooks 读取配置中的 hooks 字段，方便断言。
func readHooks(t *testing.T, path string) map[string][]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("解析 %s 失败: %v\n%s", path, err, data)
	}
	return root.Hooks
}

func countOurs(entries []json.RawMessage, agentName string) int {
	n := 0
	for _, e := range entries {
		if isOurEntry(e, hookMarker+" "+agentName) {
			n++
		}
	}
	return n
}

func TestInstallPreservesThirdPartyHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit.sh"}]}
    ],
    "Notification": [
      {"hooks": [{"type": "command", "command": "say hi"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := installHooksFile(path, "claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("首次安装应写入")
	}

	hooks := readHooks(t, path)
	if got := countOurs(hooks["PreToolUse"], "claude"); got != 1 {
		t.Fatalf("PreToolUse 自家条目 = %d", got)
	}
	if len(hooks["PreToolUse"]) != 2 {
		t.Fatalf("第三方 PreToolUse 条目丢失: %d", len(hooks["PreToolUse"]))
	}
	if !strings.Contains(string(hooks["PreToolUse"][0]), "audit.sh") {
		t.Fatalf("第三方条目内容变更: %s", hooks["PreToolUse"][0])
	}
	// 未涉及的事件原样保留。
	if len(hooks["Notification"]) != 1 || !strings.Contains(string(hooks["Notification"][0]), "say hi") {
		t.Fatalf("Notification 条目丢失: %v", hooks["Notification"])
	}
	for _, ev := range []string{"SessionStart", "PermissionRequest", "PostToolUse", "Stop", "SessionEnd"} {
		if countOurs(hooks[ev], "claude") != 1 {
			t.Fatalf("%s 缺少自家条目", ev)
		}
	}

	// 顶层未知字段保留。
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"model": "opus"`) {
		t.Fatalf("顶层字段丢失:\n%s", data)
	}

	// 幂等：第二次安装不应产生变化。
	changed, err = installHooksFile(path, "claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("重复安装不应改写文件")
	}
}

func TestUninstallKeepsThirdPartyOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit.sh"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHooksFile(path, "claude", "/opt/homebrew/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallHooksFile(path, "claude"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "devpilot-hook") {
		t.Fatalf("卸载后仍有自家条目:\n%s", data)
	}
	if !strings.Contains(string(data), "audit.sh") {
		t.Fatalf("卸载误删第三方条目:\n%s", data)
	}
}

func TestInstallModeSwitchCleansPermissionRequest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if _, err := installHooksFile(path, "claude", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := installHooksFile(path, "claude", "/bin/devpilot-hook", config.ApprovalModePreToolUse); err != nil {
		t.Fatal(err)
	}
	hooks := readHooks(t, path)
	if _, ok := hooks["PermissionRequest"]; ok {
		t.Fatalf("切换模式后 PermissionRequest 应被清理: %v", hooks["PermissionRequest"])
	}
	if countOurs(hooks["PreToolUse"], "claude") != 1 {
		t.Fatalf("PreToolUse = %v", hooks["PreToolUse"])
	}
}

func TestInstallRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	broken := []byte("{not json")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHooksFile(path, "claude", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err == nil {
		t.Fatal("非法 JSON 应拒绝写入")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(broken) {
		t.Fatalf("文件被改动:\n%s", after)
	}
}

func TestInstallCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{"hooks":{}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHooksFile(path, "codex", "/bin/devpilot-hook", config.ApprovalModePermissionRequest); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".devpilot.bak")
	if err != nil {
		t.Fatalf("缺少备份: %v", err)
	}
	if string(bak) != original {
		t.Fatalf("备份内容 = %q", bak)
	}
}

func TestCommandLineQuotesSpaces(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/devpilot-hook":       "/usr/local/bin/devpilot-hook hook claude",
		"/Users/eric/My Tools/devpilot-hook": `"/Users/eric/My Tools/devpilot-hook" hook claude`,
	}
	for exe, want := range cases {
		if got := commandLine(exe, "claude"); got != want {
			t.Fatalf("commandLine(%q) = %q, want %q", exe, got, want)
		}
	}
}

func TestIsVersionedPath(t *testing.T) {
	for _, p := range []string{
		"/opt/homebrew/Cellar/devpilot-hook/0.1.0/bin/devpilot-hook",
		"/opt/homebrew/Caskroom/devpilot-hook/0.1.0/devpilot-hook",
	} {
		if !isVersionedPath(p) {
			t.Fatalf("%s 应判定为版本目录", p)
		}
	}
	for _, p := range []string{
		"/opt/homebrew/bin/devpilot-hook",
		"/usr/local/bin/devpilot-hook",
	} {
		if isVersionedPath(p) {
			t.Fatalf("%s 不应判定为版本目录", p)
		}
	}
}
