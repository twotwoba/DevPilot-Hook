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
		fmt.Fprintln(stderr, "usage: devpilot-hook service install|uninstall|status")
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
		fmt.Fprintln(stdout, "registered auto-start service:", service.Path())
	case "uninstall":
		if err := service.Uninstall(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "removed auto-start service")
	case "status":
		status, err := service.Status()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, status)
	default:
		fmt.Fprintf(stderr, "unknown service subcommand: %s\n", fs.Arg(0))
		return 2
	}
	return 0
}
