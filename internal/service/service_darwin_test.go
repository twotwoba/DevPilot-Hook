//go:build darwin

package service

import (
	"strings"
	"testing"
)

func TestRenderPlist(t *testing.T) {
	data, err := renderPlist("/opt/homebrew/bin/devpilot-hook", "", osGetuid())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<string>com.twotwoba.devpilot-hook</string>",
		"<string>/opt/homebrew/bin/devpilot-hook</string>",
		"<string>serve</string>",
		"<string>--foreground</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("plist 缺少 %q:\n%s", want, s)
		}
	}
}

func TestRenderPlistEscapesPath(t *testing.T) {
	data, err := renderPlist("/tmp/a&b/devpilot-hook", "", osGetuid())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "a&amp;b") {
		t.Fatalf("路径未转义:\n%s", data)
	}
}

func osGetuid() int { return currentUID() }
