package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/twotwoba/DevPilot-Hook/internal/service"
)

func runService(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	pathsFlag(fs, &dir)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "用法: devpilot-hook service install|uninstall|status")
		return 2
	}

	switch fs.Arg(0) {
	case "install":
		exe, err := findStableExecutable()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := service.Install(exe, dir); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "已注册开机自启服务:", service.Path())
	case "uninstall":
		if err := service.Uninstall(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "已移除开机自启服务")
	case "status":
		status, err := service.Status()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, status)
	default:
		fmt.Fprintf(stderr, "未知 service 子命令: %s\n", fs.Arg(0))
		return 2
	}
	return 0
}
