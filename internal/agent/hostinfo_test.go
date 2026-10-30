package agent

import (
	"net"
	"testing"
	"time"
)

// TestHostInfoProviderTTL 未过期返回缓存,过期后重探。
func TestHostInfoProviderTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	p := &HostInfoProvider{interval: time.Minute, now: func() time.Time { return now }}
	p.info = HostInfo{Hostname: "stale-host"}
	p.stamp = base

	// 未到刷新周期:原样返回缓存。
	if got := p.Get(); got.Hostname != "stale-host" {
		t.Fatalf("TTL 内应返回缓存,得到 %q", got.Hostname)
	}

	// 推进时钟越过周期:触发重探,拿到真实 hostname。
	now = base.Add(2 * time.Minute)
	got := p.Get()
	if got.Hostname == "stale-host" || got.Hostname == "" {
		t.Fatalf("过期后应重探,得到 %q", got.Hostname)
	}
	// 重探后回到 TTL 内,再次 Get 不应改变时间戳语义(仍是新快照)。
	if again := p.Get(); again != got {
		t.Fatalf("同周期内两次 Get 应一致: %+v vs %+v", again, got)
	}
}

// TestPrimaryIPv4PrivateOrEmpty 锁死不变量:要么探测失败为空,
// 要么必须是内网 IPv4(绝不回填公网/环回地址)。
func TestPrimaryIPv4PrivateOrEmpty(t *testing.T) {
	ipStr := primaryIPv4()
	if ipStr == "" {
		return // 容器/无内网环境合法
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		t.Fatalf("primaryIPv4 返回非 IPv4: %q", ipStr)
	}
	if ip.IsLoopback() {
		t.Fatalf("不得返回环回地址: %q", ipStr)
	}
	for _, n := range privateIPv4Nets {
		if n.Contains(ip.To4()) {
			return
		}
	}
	t.Fatalf("返回了非内网地址: %q", ipStr)
}
