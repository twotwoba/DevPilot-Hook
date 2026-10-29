//go:build linux

package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitName = "devpilot-hook.service"

// Path 返回 systemd user unit 路径。
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", unitName)
}

// Install 写入 user unit 并启用。
func Install(exe, dir string) error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, renderUnit(exe, dir), 0o600); err != nil {
		return fmt.Errorf("failed to write systemd unit: %w", err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall 停用并删除 unit。
func Uninstall() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	err := os.Remove(Path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

// Status 查询 systemd 状态。
func Status() (string, error) {
	out, err := exec.Command("systemctl", "--user", "status", unitName, "--no-pager").CombinedOutput()
	if err != nil {
		return "not running (run service install)", nil
	}
	return string(out), nil
}

// renderUnit 生成 systemd user unit 内容。
func renderUnit(exe, dir string) []byte {
	execStart := exe
	if strings.ContainsAny(exe, " \t") {
		execStart = `"` + exe + `"`
	}
	for _, a := range serviceArgs(dir) {
		execStart += " " + a
	}
	return fmt.Appendf(nil, `[Unit]
Description=DevPilot Hook daemon
After=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=3
Environment=HOME=%s

[Install]
WantedBy=default.target
`, execStart, os.Getenv("HOME"))
}
