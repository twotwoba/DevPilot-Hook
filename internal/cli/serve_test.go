package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// entryTimeouts 展开一个 hook 条目里所有 command 的 timeout。
func entryTimeouts(t *testing.T, raw json.RawMessage) []int {
	t.Helper()
	var e struct {
		Hooks []struct {
			Timeout int `json:"timeout"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	out := make([]int, 0, len(e.Hooks))
	for _, h := range e.Hooks {
		out = append(out, h.Timeout)
	}
	return out
}

// TestSelfHealHooksUpgradesStaleTimeout 复现 brew 升级场景:
// hooks 文件里还是旧的超时,serve 启动时幂等重写为新默认(+60)。
func TestSelfHealHooksUpgradesStaleTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// 稳定可执行文件:PATH 前插假 bin(非版本目录,模拟 /opt/homebrew/bin)。
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "devpilot-hook"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := config.Default()
	cfg.Agents = []string{"claude"}

	var stderr bytes.Buffer
	selfHealHooks(cfg, &stderr)
	if !strings.Contains(stderr.String(), "refreshed claude hooks") {
		t.Fatalf("首跑应写入 hooks: %q", stderr.String())
	}

	path := filepath.Join(home, ".claude", "settings.json")
	hooks := readHooks(t, path)
	if got := entryTimeouts(t, hooks["PermissionRequest"][0]); len(got) != 1 || got[0] != config.DefaultApprovalTimeoutSeconds+60 {
		t.Fatalf("新装 timeout = %v, want [%d]", got, config.DefaultApprovalTimeoutSeconds+60)
	}

	// 手工把 timeout 改回旧值 300(brew 升级只换二进制、不重跑 install 的现场)。
	var root map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	entries := root["hooks"].(map[string]any)["PermissionRequest"].([]any)
	entry := entries[0].(map[string]any)
	entry["hooks"].([]any)[0].(map[string]any)["timeout"] = 300
	patched, _ := json.Marshal(root)
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	stderr.Reset()
	selfHealHooks(cfg, &stderr)
	if !strings.Contains(stderr.String(), "refreshed claude hooks") {
		t.Fatalf("陈旧 timeout 应触发重写: %q", stderr.String())
	}
	hooks = readHooks(t, path)
	if got := entryTimeouts(t, hooks["PermissionRequest"][0]); len(got) != 1 || got[0] != config.DefaultApprovalTimeoutSeconds+60 {
		t.Fatalf("自愈后 timeout = %v, want [%d]", got, config.DefaultApprovalTimeoutSeconds+60)
	}

	// 再跑一次:内容一致,不再报 refreshed。
	stderr.Reset()
	selfHealHooks(cfg, &stderr)
	if strings.Contains(stderr.String(), "refreshed") {
		t.Fatalf("幂等:内容一致不应重写: %q", stderr.String())
	}
}
