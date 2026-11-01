package agent

import (
	"errors"
	"testing"
)

// stubHostSources 注入内核名与 scutil 输出,返回还原函数。
func stubHostSources(t *testing.T, kernel string, scutil map[string]string) func() {
	t.Helper()
	origKernel, origScutil := kernelHostname, scutilValue
	kernelHostname = func() string { return kernel }
	scutilValue = func(key string) (string, error) {
		if v, ok := scutil[key]; ok {
			return v, nil
		}
		return "", errors.New("not set")
	}
	return func() { kernelHostname, scutilValue = origKernel, origScutil }
}

// TestSanitizeBogus 伪值清空、尾部空点号褪掉、正常名(含 .local)原样保留。
func TestSanitizeBogus(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bogon", ""},
		{"Bogon", ""},
		{"localhost", ""},
		{"localhost.localdomain", ""},
		{"", ""},
		{"  ", ""},
		{"Mac.", "Mac"},
		{"  Mac  ", "Mac"},
		{"Mac.local", "Mac.local"}, // 路由契约:后缀留给精确比对,此处不截
	}
	for _, c := range cases {
		if got := sanitizeBogus(c.in); got != c.want {
			t.Fatalf("sanitizeBogus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDisplayNamePrefersFriendlyName 展示名优先 scutil 友好名,bogon 内核值不外泄。
func TestDisplayNamePrefersFriendlyName(t *testing.T) {
	defer stubHostSources(t, "bogon", map[string]string{
		"ComputerName": "Eric's MacBook Pro\n",
	})()
	if got := DisplayName(); got != "Eric's MacBook Pro" {
		t.Fatalf("应取 ComputerName 友好名,得到 %q", got)
	}
}

// TestDisplayNameFallsToLocalHostName ComputerName 缺位时退 LocalHostName。
func TestDisplayNameFallsToLocalHostName(t *testing.T) {
	defer stubHostSources(t, "bogon", map[string]string{
		"LocalHostName": "Erics-MacBook-Pro",
	})()
	if got := DisplayName(); got != "Erics-MacBook-Pro" {
		t.Fatalf("应取 LocalHostName,得到 %q", got)
	}
}

// TestDisplayNameKernelFallback scutil 全缺位时用内核名并褪 .local 后缀。
func TestDisplayNameKernelFallback(t *testing.T) {
	defer stubHostSources(t, "Erics-MacBook-Pro.local", nil)()
	if got := DisplayName(); got != "Erics-MacBook-Pro" {
		t.Fatalf("应取内核名并褪 .local,得到 %q", got)
	}
}

// TestDisplayNameConstantFallback 全链路无值时用固定名,绝不返回 bogon。
func TestDisplayNameConstantFallback(t *testing.T) {
	defer stubHostSources(t, "bogon", nil)()
	if got := DisplayName(); got != "devpilot-daemon" {
		t.Fatalf("应取固定名,得到 %q", got)
	}
}

// TestRoutableHostname 路由名滤伪值但保留 .local(与 HostRecord 精确比对)。
func TestRoutableHostname(t *testing.T) {
	defer stubHostSources(t, "bogon", nil)()
	if got := RoutableHostname(); got != "" {
		t.Fatalf("bogon 应滤成空,得到 %q", got)
	}
	kernelHostname = func() string { return "Erics-MacBook-Pro.local" }
	if got := RoutableHostname(); got != "Erics-MacBook-Pro.local" {
		t.Fatalf("路由名应保留 .local 原样,得到 %q", got)
	}
}
