//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const label = "com.twotwoba.devpilot-hook"

// Path 返回 LaunchAgent plist 路径。
func Path() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// Install 写入 LaunchAgent 并加载。
func Install(exe, dir string) error {
	plist, err := renderPlist(exe, dir, currentUID())
	if err != nil {
		return err
	}
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, plist, 0o600); err != nil {
		return fmt.Errorf("failed to write LaunchAgent plist: %w", err)
	}

	// 重复安装时先卸载旧实例；bootout 失败说明尚未加载，可以忽略。
	domain := "gui/" + strconv.Itoa(currentUID())
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall 卸载并删除 plist。
func Uninstall() error {
	domain := "gui/" + strconv.Itoa(currentUID())
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	err := os.Remove(Path())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Status 返回 launchctl 的状态输出。
func Status() (string, error) {
	domain := "gui/" + strconv.Itoa(currentUID())
	out, err := exec.Command("launchctl", "print", domain+"/"+label).CombinedOutput()
	if err != nil {
		return "not loaded (run service install)", nil
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 12 {
		lines = lines[:12]
	}
	return strings.Join(lines, "\n"), nil
}

func currentUID() int { return os.Getuid() }

// renderPlist 生成 LaunchAgent 内容。
func renderPlist(exe, dir string, uid int) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(home, "Library", "Logs", "devpilot-hook.log")
	args := make([]string, 0, 5)
	args = append(args, exe)
	args = append(args, serviceArgs(dir)...)

	var argXML strings.Builder
	for _, a := range args {
		argXML.WriteString("<string>")
		if err := xml.EscapeText(&argXML, []byte(a)); err != nil {
			return nil, err
		}
		argXML.WriteString("</string>\n            ")
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
            %s</array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`, label, argXML.String(), logPath, logPath, home)
	_ = uid
	return buf.Bytes(), nil
}
