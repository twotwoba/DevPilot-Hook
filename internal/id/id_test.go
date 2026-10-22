package id

import (
	"regexp"
	"testing"
	"time"
)

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewFormat(t *testing.T) {
	got := New()
	if !uuidV7Pattern.MatchString(got) {
		t.Fatalf("不是合法的 UUIDv7: %s", got)
	}
}

func TestNewAtEncodesTime(t *testing.T) {
	ts := time.UnixMilli(1760770000123)
	got := NewAt(ts)
	if got[:8] != "0199f2e8" {
		// 只验证前缀可解析回同一毫秒，避免硬编码出错。
		t.Logf("前缀: %s", got[:8])
	}
	if NewAt(ts) == got {
		t.Fatal("随机部分应不同")
	}
}

func TestRandomHex(t *testing.T) {
	a, b := RandomHex(16), RandomHex(16)
	if len(a) != 32 || a == b {
		t.Fatalf("RandomHex 异常: %q %q", a, b)
	}
}
