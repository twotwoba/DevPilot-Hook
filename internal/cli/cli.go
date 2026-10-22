// Package cli 实现 devpilot-hook 的所有子命令。
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Version 由构建时注入（goreleaser -ldflags）。
var Version = "dev"

// Run 是命令入口，返回进程退出码。
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "hook":
		return runHook(args[1:], stdin, stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "install":
		return runInstall(args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(args[1:], stdout, stderr)
	case "pair":
		return runPair(args[1:], stdout, stderr)
	case "unpair":
		return runUnpair(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "service":
		return runService(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令: %s\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `devpilot-hook —— DevPilot 的本地 hook 守护进程

用法:
  devpilot-hook install    [--agent claude,codex] [--approval-mode permission-request|pre-tool-use] [--dir PATH]
  devpilot-hook uninstall  [--agent claude,codex] [--purge] [--dir PATH]
  devpilot-hook pair       --relay https://relay.example.com [--dir PATH]
  devpilot-hook unpair     [--dir PATH]
  devpilot-hook serve      [--dir PATH] [--foreground]
  devpilot-hook doctor     [--json] [--dir PATH]
  devpilot-hook service    install|uninstall|status [--dir PATH]
  devpilot-hook hook       claude|codex     （由 Agent 配置调用，读 stdin）
  devpilot-hook version
`)
}

// pathsFlag 为子命令注册 --dir 并解析。
func pathsFlag(fs *flag.FlagSet, dir *string) {
	fs.StringVar(dir, "dir", "", "状态目录（默认使用用户配置目录）")
}

// resolvePaths 返回指定目录或默认目录。
func resolvePaths(dir string) (config.Paths, error) {
	if dir != "" {
		return config.NewPaths(dir), nil
	}
	return config.DefaultPaths()
}

// homeFile 返回用户主目录下的路径。
func homeFile(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cli: 无法定位用户主目录: %w", err)
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// parseAgents 解析逗号分隔的 agent 列表。
func parseAgents(s string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "":
			continue
		case "claude", "codex":
			out = append(out, part)
		default:
			return nil, fmt.Errorf("未知 agent: %s（只支持 claude、codex）", part)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少选择一个 agent")
	}
	return out, nil
}
