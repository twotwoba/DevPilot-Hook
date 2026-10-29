package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// hookMarker 用于识别我们写入的 hook 条目，卸载时只删自己的。
const hookMarker = "devpilot-hook hook"

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

type hookEvent struct {
	Event   string
	Matcher string
}

// allHookEvents 是某个 Agent 可能涉及的全部事件。
//
// 清理自家条目时要遍历全集，否则从 permission-request 切到 pre-tool-use 后，
// 旧的 PermissionRequest 条目会残留并造成重复询问。
func allHookEvents(agentName string) []hookEvent {
	switch agentName {
	case "claude":
		return []hookEvent{
			{"SessionStart", "*"},
			{"PreToolUse", "*"},
			{"PermissionRequest", "*"},
			{"PostToolUse", "*"},
			{"Stop", ""},
			{"SessionEnd", ""},
		}
	case "codex":
		return []hookEvent{
			{"SessionStart", "*"},
			{"PreToolUse", "*"},
			{"PermissionRequest", "*"},
			{"PostToolUse", "*"},
			{"Stop", ""},
		}
	default:
		return nil
	}
}

// desiredHookEvents 返回某个 Agent 在当前模式下需要注册的事件。
func desiredHookEvents(agentName string, mode config.ApprovalMode) []hookEvent {
	all := allHookEvents(agentName)
	out := make([]hookEvent, 0, len(all))
	for _, ev := range all {
		if ev.Event == "PermissionRequest" && agentName == "claude" && mode != config.ApprovalModePermissionRequest {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// installHooksFile 把 devpilot-hook 条目合并进 Agent 的 hooks 配置文件。
//
// 只增改自家条目：其余 key、其余 hook 条目按原始 JSON 保留；
// 解析失败时拒绝写入，避免破坏用户配置。
func installHooksFile(path, agentName, exe string, mode config.ApprovalMode) (bool, error) {
	return mergeHooksFile(path, agentName, exe, mode, true)
}

// uninstallHooksFile 只删除自家条目。
func uninstallHooksFile(path, agentName string) (bool, error) {
	return mergeHooksFile(path, agentName, "", "", false)
}

func mergeHooksFile(path, agentName, exe string, mode config.ApprovalMode, install bool) (bool, error) {
	root := map[string]json.RawMessage{}
	original, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if exists && len(bytes.TrimSpace(original)) > 0 {
		if err := json.Unmarshal(original, &root); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; refusing to modify: %w", path, err)
		}
	}

	hooks := map[string][]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return false, fmt.Errorf("malformed hooks field in %s; refusing to modify: %w", path, err)
		}
	}

	marker := hookMarker + " " + agentName
	desired := make(map[string]bool)
	for _, ev := range desiredHookEvents(agentName, mode) {
		desired[ev.Event] = true
	}

	for _, ev := range allHookEvents(agentName) {
		kept := make([]json.RawMessage, 0, len(hooks[ev.Event])+1)
		for _, entry := range hooks[ev.Event] {
			if !isOurEntry(entry, marker) {
				kept = append(kept, entry)
			}
		}
		if install && desired[ev.Event] {
			entry, err := json.Marshal(hookMatcher{
				Matcher: ev.Matcher,
				Hooks: []hookCommand{{
					Type:    "command",
					Command: commandLine(exe, agentName),
					Timeout: config.DefaultApprovalTimeoutSeconds + 60,
				}},
			})
			if err != nil {
				return false, err
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, ev.Event)
		} else {
			hooks[ev.Event] = kept
		}
	}

	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		data, err := json.Marshal(hooks)
		if err != nil {
			return false, err
		}
		root["hooks"] = data
	}

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	data = append(data, '\n')
	if exists && bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(data)) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if exists {
		bak := path + ".devpilot.bak"
		if err := os.WriteFile(bak, original, 0o600); err != nil {
			return false, fmt.Errorf("failed to back up %s: %w", path, err)
		}
	}
	if err := config.WriteAtomic(path, data, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// isOurEntry 判断一条 matcher 条目是否由本工具写入。
func isOurEntry(raw json.RawMessage, marker string) bool {
	var entry hookMatcher
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	for _, h := range entry.Hooks {
		if strings.Contains(h.Command, marker) {
			return true
		}
	}
	return false
}

// commandLine 生成稳定的 hook 命令；路径含空格时加引号。
func commandLine(exe, agentName string) string {
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	return exe + " hook " + agentName
}

// findStableExecutable 优先返回不在版本目录里的稳定路径。
//
// Homebrew 的 bin 链接在升级后继续有效；直接使用 Cellar/Caskroom
// 版本目录里的真实路径会在升级后失效。
func findStableExecutable() (string, error) {
	if p, err := exec.LookPath("devpilot-hook"); err == nil && !isVersionedPath(p) {
		// 保留 bin 下的符号链接本身，不要 EvalSymlinks 到 Cellar 版本目录。
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if isVersionedPath(exe) {
		return "", fmt.Errorf("executable is in a versioned directory %s; run install from a stable Homebrew path (e.g. /opt/homebrew/bin/devpilot-hook)", exe)
	}
	return exe, nil
}

// isVersionedPath 判断路径是否落在会随升级消失的版本目录中。
func isVersionedPath(p string) bool {
	slashed := filepath.ToSlash(p)
	return strings.Contains(slashed, "/Cellar/") || strings.Contains(slashed, "/Caskroom/")
}

// agentConfigPath 返回 Agent hooks 配置文件路径。
func agentConfigPath(agentName string) (string, error) {
	switch agentName {
	case "claude":
		return homeFile(".claude", "settings.json")
	case "codex":
		return homeFile(".codex", "hooks.json")
	default:
		return "", fmt.Errorf("unknown agent: %s", agentName)
	}
}
