//go:build linux

package service

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	s := string(renderUnit("/usr/local/bin/devpilot-hook", "/tmp/state"))
	for _, want := range []string{
		"ExecStart=/usr/local/bin/devpilot-hook serve --foreground --dir /tmp/state",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("unit 缺少 %q:\n%s", want, s)
		}
	}
}
