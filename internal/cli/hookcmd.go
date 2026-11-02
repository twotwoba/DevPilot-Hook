package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// tmuxProbeTimeout 是 tmux display-message 的兜底超时:
// 正常是本地 unix socket 往返(毫秒级),超时只为防 tmux 服务端卡死拖住 hook。
// 生产固定 2s;提成 var 仅供测试在整套 -race 并行、进程启动被饿时放宽。
var tmuxProbeTimeout = 2 * time.Second

// hookClientTimeout 由审批等待窗派生:+50s,且必须小于 install 写入的
// hook timeout(+60s)——保证 daemon 的「无决策」空响应先于 Agent 侧
// hook 超时到达,Claude Code 回落原生询问而不是报 hook 失败。
func hookClientTimeout(cfg *config.Config) time.Duration {
	return time.Duration(cfg.ApprovalTimeout+50) * time.Second
}

func runHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	pathsFlag(fs, &dir)
	if err := fs.Parse(args); err != nil {
		return 0 // 绝不因为参数问题破坏 Agent
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "claude" && fs.Arg(0) != "codex") {
		return 0
	}
	agentName := fs.Arg(0)

	paths, err := resolvePaths(dir)
	if err != nil {
		return 0
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return 0
	}
	token, err := os.ReadFile(paths.TokenPath())
	if err != nil {
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}

	base := statusURL(cfg)
	if override := os.Getenv("DEVPILOT_HOOK_GATEWAY"); override != "" {
		base = strings.TrimRight(override, "/")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClientTimeout(cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/hook/"+agentName, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/json")
	if info := tmuxProbe(body, cfg); info.session != "" {
		req.Header.Set("X-DevPilot-Tmux-Session", info.session)
		if info.window != "" {
			req.Header.Set("X-DevPilot-Tmux-Window", info.window)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	// 只有明确的 200 才输出决策；其余一律无输出，让 Agent 走原生流程。
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(stdout, resp.Body)
	}
	return 0
}

// tmuxInfo 是一次探测取回的 tmux 定位:会话名 + 窗口号(v0.3.1)。
type tmuxInfo struct {
	session string
	window  string
}

// tmuxProbe 探测当前 tmux 会话名与窗口号(经专用 header 传给 daemon,由
// 网关盖进 payload,App 收件箱据此路由回会话/展示详情)。只对审批候选
// 事件探测:hook 调用频繁(每个工具用前/后各一次),无条件探测会被卡死
// 的 tmux 服务端拖累。$TMUX 由 tmux 服务端注入;非 tmux 环境为空,直接
// 跳过。单次 display-message 同取 '#S #{window_index}'。
func tmuxProbe(body []byte, cfg *config.Config) tmuxInfo {
	if os.Getenv("TMUX") == "" {
		return tmuxInfo{}
	}
	in, err := agent.ParseInput(body)
	if err != nil {
		return tmuxInfo{}
	}
	approvalCandidate := in.HookEventName == "PermissionRequest" ||
		(in.HookEventName == "PreToolUse" &&
			config.ApprovalMode(cfg.ApprovalMode) == config.ApprovalModePreToolUse)
	if !approvalCandidate {
		return tmuxInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S #{window_index}").Output()
	if err != nil {
		return tmuxInfo{} // 失败回落:不带 header,网关按无会话处理
	}
	return parseTmuxInfo(strings.TrimSpace(string(out)))
}

// parseTmuxInfo 解析 display-message 输出:会话名可含空格,不能天真
// Split——从行尾取最后一个空格分隔 token 为窗口号(全数字校验,非数字
// 置空),其余全量保留为会话名。
func parseTmuxInfo(line string) tmuxInfo {
	// 自防御 trim:调用方通常已 TrimSpace,但直接喂原始输出也不出错
	line = strings.TrimSpace(line)
	if line == "" {
		return tmuxInfo{}
	}
	idx := strings.LastIndexByte(line, ' ')
	if idx < 0 {
		// 只有一段:按旧协议视作纯会话名
		return tmuxInfo{session: line}
	}
	session := strings.TrimSpace(line[:idx])
	window := strings.TrimSpace(line[idx+1:])
	for _, r := range window {
		if r < '0' || r > '9' {
			window = "" // 非数字(旧 tmux/异常输出):窗口号置空,余下段保留为会话名
			break
		}
	}
	return tmuxInfo{session: session, window: window}
}
