package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenMissing(t *testing.T) {
	paths := NewPaths(t.TempDir())
	cfg, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GatewayPort != DefaultGatewayPort || cfg.ApprovalMode != DefaultApprovalMode {
		t.Fatalf("默认值不符: %+v", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	paths := NewPaths(t.TempDir())
	cfg := Default()
	if !cfg.EnsureIdentity() {
		t.Fatal("首次应生成 daemon_id")
	}
	cfg.RelayURL = "https://relay.example.com"
	if err := cfg.Save(paths); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(paths.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限应为 0600，实际 %v", info.Mode().Perm())
	}

	got, err := Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if got.DaemonID != cfg.DaemonID || got.RelayURL != cfg.RelayURL {
		t.Fatalf("往返不一致: %+v", got)
	}
}

func TestLoadRejectsBrokenJSON(t *testing.T) {
	paths := NewPaths(t.TempDir())
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(paths); err == nil {
		t.Fatal("损坏的配置必须报错，不能静默覆盖")
	}
}

func TestTokenStableAndPrivate(t *testing.T) {
	paths := NewPaths(t.TempDir())
	a, err := LoadOrCreateToken(paths)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateToken(paths)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 64 {
		t.Fatalf("token 应稳定且为 64 位 hex: %q %q", a, b)
	}
	info, err := os.Stat(paths.TokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token 权限应为 0600，实际 %v", info.Mode().Perm())
	}
}

func TestWriteAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := WriteAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("原子替换失败: %s", data)
	}
}

func TestNormalizeRejectsBadMode(t *testing.T) {
	cfg := Default()
	cfg.ApprovalMode = "whatever"
	if err := cfg.Normalize(); err == nil {
		t.Fatal("未知模式应报错")
	}
}
