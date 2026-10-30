// 主机身份探测:随事件上报 hostname / 内网 IP / 用户名,
// 供 App 收件箱把通知路由回 HostRecord 与 tmux 会话。
package agent

import (
	"net"
	"os"
	"os/user"
	"sync"
	"time"
)

// hostInfoRefreshInterval 是主机信息快照的刷新周期。
// 探测有系统调用成本,而网络环境(DHCP/VPN 切换)分钟级变化,
// 5 分钟足够新鲜;调用方是低频审批链路,同步重探可承受。
const hostInfoRefreshInterval = 5 * time.Minute

// HostInfo 是一次主机身份快照;全部字段尽力探测,失败留空,
// 绝不因此阻塞 hook 链路。
type HostInfo struct {
	Hostname  string // os.Hostname()
	PrimaryIP string // 首个非环回内网 IPv4(RFC1918 + CGNAT/Tailscale)
	User      string // 当前用户名
}

// HostInfoProvider 带 TTL 惰性刷新的主机信息快照(并发安全)。
type HostInfoProvider struct {
	interval time.Duration
	now      func() time.Time

	mu    sync.Mutex
	info  HostInfo
	stamp time.Time
}

// NewHostInfoProvider 启动时探测一次,之后按 TTL 惰性刷新。
func NewHostInfoProvider() *HostInfoProvider {
	p := &HostInfoProvider{interval: hostInfoRefreshInterval, now: time.Now}
	p.info = collectHostInfo()
	p.stamp = p.now()
	return p
}

// Get 返回快照;超过刷新周期时同步重探。
func (p *HostInfoProvider) Get() HostInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.now().Sub(p.stamp) >= p.interval {
		p.info = collectHostInfo()
		p.stamp = p.now()
	}
	return p.info
}

// collectHostInfo 一次性探测;任何一步失败都不影响其余字段。
func collectHostInfo() HostInfo {
	var h HostInfo
	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
	}
	h.PrimaryIP = primaryIPv4()
	if u, err := user.Current(); err == nil && u.Username != "" {
		h.User = u.Username
	}
	return h
}

// privateIPv4Nets 判定「内网」的网段表:RFC1918 加 CGNAT 100.64/10——
// Tailscale 等覆盖网格的地址常是手机到主机的唯一可路由路径,一并采纳。
var privateIPv4Nets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// primaryIPv4 取首个「接口已启用、非环回」的内网 IPv4。
// 按系统枚举序取第一个:macOS/Linux 的默认路由接口(en0/eth0)几乎总在前,
// 无需引入平台相关的路由表查询。容器/无内网环境返回空串。
func primaryIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			for _, n := range privateIPv4Nets {
				if n.Contains(ip4) {
					return ip4.String()
				}
			}
		}
	}
	return ""
}
