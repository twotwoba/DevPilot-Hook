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
	if name := tmuxProbe(body, cfg); name != "" {
		req.Header.Set("X-DevPilot-Tmux-Session", name)
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

// tmuxProbe 探测当前 tmux 会话名(经专用 header 传给 daemon,由网关盖进
// payload,App 收件箱据此路由回会话)。只对审批候选事件探测:hook 调用
// 频繁(每个工具用前/后各一次),无条件探测会被卡死的 tmux 服务端拖累。
// $TMUX 由 tmux 服务端注入;非 tmux 环境为空,直接跳过。
func tmuxProbe(body []byte, cfg *config.Config) string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	in, err := agent.ParseInput(body)
	if err != nil {
		return ""
	}
	approvalCandidate := in.HookEventName == "PermissionRequest" ||
		(in.HookEventName == "PreToolUse" &&
			config.ApprovalMode(cfg.ApprovalMode) == config.ApprovalModePreToolUse)
	if !approvalCandidate {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return "" // 失败回落:不带 header,网关按无会话处理
	}
	return strings.TrimSpace(string(out))
}
