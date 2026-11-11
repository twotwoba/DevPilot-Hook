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
	fs.StringVar(&agentsFlag, "agent", "claude,codex", "agents to install (comma-separated: claude, codex, opencode)")
	fs.StringVar(&modeFlag, "approval-mode", string(config.ApprovalModePermissionRequest), "approval mode: permission-request or pre-tool-use")
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
		fmt.Fprintf(stderr, "unknown approval mode: %s\n", modeFlag)
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
		if name == "opencode" {
			// OpenCode 无 hooks 配置文件，安装的是全局插件文件；目录与
			// API 形态按本机 opencode 版本选择（0.x 与 2.x 不兼容）。
			changed, variant, path, err := installOpenCodePlugin(exe)
			if err != nil {
				fmt.Fprintf(stderr, "failed to install %s plugin: %v\n", name, err)
				return 1
			}
			note := openCodeVariantNote(variant)
			switch {
			case changed:
				fmt.Fprintf(stdout, "installed %s plugin (%s%s) -> %s\n", name, variant, note, path)
			default:
				fmt.Fprintf(stdout, "%s plugin already up to date (%s%s) -> %s\n", name, variant, note, path)
			}
			continue
		}
		path, err := agentConfigPath(name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		changed, err := installHooksFile(path, name, exe, mode)
		if err != nil {
			fmt.Fprintf(stderr, "failed to install %s hooks: %v\n", name, err)
			return 1
		}
		switch {
		case changed:
			fmt.Fprintf(stdout, "installed %s hooks -> %s (original backed up as .devpilot.bak)\n", name, path)
		default:
			fmt.Fprintf(stdout, "%s hooks already up to date -> %s\n", name, path)
		}
	}

	fmt.Fprintf(stdout, "state directory: %s\n", paths.Dir)
	fmt.Fprintln(stdout, "next: devpilot-hook pair --relay https://<your relay host>")
	fmt.Fprintln(stdout, "then: devpilot-hook service install")
	return 0
}

func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir, agentsFlag string
	var purge bool
	pathsFlag(fs, &dir)
	fs.StringVar(&agentsFlag, "agent", "claude,codex,opencode", "agents to uninstall (comma-separated)")
	fs.BoolVar(&purge, "purge", false, "also remove the state directory (credentials, queue, token)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agents, err := parseAgents(agentsFlag)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	for _, name := range agents {
		if name == "opencode" {
			changed, removed, err := uninstallOpenCodePlugin()
			if err != nil {
				fmt.Fprintf(stderr, "failed to uninstall %s plugin: %v\n", name, err)
				return 1
			}
			if changed {
				for _, p := range removed {
					fmt.Fprintf(stdout, "removed %s plugin from %s\n", name, p)
				}
			} else {
				fmt.Fprintf(stdout, "no devpilot-hook plugin found for %s\n", name)
			}
			continue
		}
		path, err := agentConfigPath(name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		changed, err := uninstallHooksFile(path, name)
		if err != nil {
			fmt.Fprintf(stderr, "failed to uninstall %s hooks: %v\n", name, err)
			return 1
		}
		if changed {
			fmt.Fprintf(stdout, "removed %s hooks from %s\n", name, path)
		} else {
			fmt.Fprintf(stdout, "no devpilot-hook entry found for %s\n", name)
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
		fmt.Fprintf(stdout, "purged state directory: %s\n", paths.Dir)
	}
	return 0
}
