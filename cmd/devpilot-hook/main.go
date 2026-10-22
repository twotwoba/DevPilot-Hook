// Package main 是 devpilot-hook 的入口。
package main

import (
	"os"

	"github.com/twotwoba/DevPilot-Hook/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
