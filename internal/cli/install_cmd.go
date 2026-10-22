package cli

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir, agentsFlag, modeFlag string
	pathsFlag(fs, &dir)
	fs.StringVar(&agentsFlag, "agent", "claude,codex", "要安装的 Agent（逗号分隔）")
	fs.StringVar(&modeFlag, "approval-mode", string(config.ApprovalModePermissionRequest), "审批模式：permission-request 或 pre-tool-use")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agents, err := parseAgents(agentsFlag)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	mode := config.ApprovalMode(modeFlag)
	if !mode.Valid() {
		fmt.Fprintf(stderr, "未知审批模式: %s\n", modeFlag)
		return 2
	}

	paths, err := resolvePaths(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, err := config.Load(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg.EnsureIdentity()
	cfg.ApprovalMode = string(mode)
	cfg.Agents = agents
	if err := cfg.Save(paths); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := config.LoadOrCreateToken(paths); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	exe, err := findStableExecutable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	for _, name := range agents {
		path, err := agentConfigPath(name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		changed, err := installHooksFile(path, name, exe, mode)
		if err != nil {
			fmt.Fprintf(stderr, "安装 %s hooks 失败: %v\n", name, err)
			return 1
		}
		switch {
		case changed:
			fmt.Fprintf(stdout, "已安装 %s hooks → %s（原文件备份为 .devpilot.bak）\n", name, path)
		default:
			fmt.Fprintf(stdout, "%s hooks 已是最新 → %s\n", name, path)
		}
	}

	fmt.Fprintf(stdout, "状态目录: %s\n", paths.Dir)
	fmt.Fprintln(stdout, "下一步: devpilot-hook pair --relay https://<你的 relay 域名>")
	fmt.Fprintln(stdout, "随后: devpilot-hook service install")
	return 0
}

func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir, agentsFlag string
	var purge bool
	pathsFlag(fs, &dir)
	fs.StringVar(&agentsFlag, "agent", "claude,codex", "要卸载的 Agent（逗号分隔）")
	fs.BoolVar(&purge, "purge", false, "同时删除状态目录（凭据、队列、token）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agents, err := parseAgents(agentsFlag)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	for _, name := range agents {
		path, err := agentConfigPath(name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		changed, err := uninstallHooksFile(path, name)
		if err != nil {
			fmt.Fprintf(stderr, "卸载 %s hooks 失败: %v\n", name, err)
			return 1
		}
		if changed {
			fmt.Fprintf(stdout, "已移除 %s hooks ← %s\n", name, path)
		} else {
			fmt.Fprintf(stdout, "%s 未发现 devpilot-hook 条目\n", name)
		}
	}

	if purge {
		paths, err := resolvePaths(dir)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.RemoveAll(paths.Dir); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "已删除状态目录: %s\n", paths.Dir)
	}
	return 0
}
