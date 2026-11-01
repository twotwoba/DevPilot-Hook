// 主机名取值分层:macOS 的内核 hostname 在无反向 DNS 的网络窗口里会被
// 系统回落成 "bogon"(DHCP 不下发名字 + RDNS 解析失败时的兜底值),而
// 配对名与事件上报都源自这里,必须按消费方分层取用:
//   - DisplayName(展示名,配对 daemon_name 用):优先 scutil 的
//     ComputerName/LocalHostName 友好名,内核值只作兜底;
//   - RoutableHostname(路由名,事件 hostname 用):只能是可解析的内核
//     hostname(滤伪值后原样上报)。展示名带空格/撇号不可解析,绝不能进
//     路由链路——App 侧 matchHost 拿它与 HostRecord.hostname 精确比对,
//     补建主机的地址预填也直接拿它当 SSH 目标。
package agent

import (
	"os"
	"os/exec"
	"strings"
)

// bogusHostnames 是解析链路可能回落出的伪主机名:不可解析、零信息,
// 上报或入库只会污染路由与展示。
var bogusHostnames = map[string]bool{
	"":                      true,
	"bogon":                 true,
	"localhost":             true,
	"localhost.localdomain": true,
}

// kernelHostname / scutilValue 独立成变量供测试注入(同 privateIPv4Nets 惯例)。
var kernelHostname = func() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

var scutilValue = func(key string) (string, error) {
	out, err := exec.Command("scutil", "--get", key).Output()
	return string(out), err
}

// sanitizeBogus 只滤伪值与尾部空点号,不改名字本身:路由链路靠它与
// HostRecord.hostname 精确比对,截 ".local" 后缀会拆掉既有匹配。
func sanitizeBogus(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.TrimSuffix(name, ".")
	if bogusHostnames[strings.ToLower(name)] {
		return ""
	}
	return name
}

// RoutableHostname 返回事件上报用的可解析主机名;伪值清空——宁可留空
// 让 IP 线索接棒(App 侧退 hostIP 匹配、预填拿 IP),也不能报个 bogon 上去。
func RoutableHostname() string {
	return sanitizeBogus(kernelHostname())
}

// DisplayName 返回机器的展示名(配对 daemon_name):scutil 友好名优先
// (ComputerName → LocalHostName,非 macOS 上 scutil 不存在自然落空),
// 内核 hostname 只作兜底,全空则用固定名。末尾褪 ".local"——展示更短,
// 且恰好喂饱 App 侧 hasHost 的前缀比对(存档 hostname 常带 .local 后缀)。
func DisplayName() string {
	for _, key := range []string{"ComputerName", "LocalHostName"} {
		out, err := scutilValue(key)
		if err != nil {
			continue
		}
		if name := sanitizeBogus(out); name != "" {
			return strings.TrimSuffix(name, ".local")
		}
	}
	if name := sanitizeBogus(kernelHostname()); name != "" {
		return strings.TrimSuffix(name, ".local")
	}
	return "devpilot-daemon"
}
