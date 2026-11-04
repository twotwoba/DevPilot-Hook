package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
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
// 网关盖进 payload,App 收件箱据此路由回会话/展示详情)。探测名单 =
// 需要 tmux 定位的事件:审批候选(PermissionRequest、pre-tool-use 审批
// 模式的 PreToolUse)+ 回合结束(Stop→TaskComplete,回复摘要卡同要路由
// 回 tmux 会话,否则收件箱把它当非 tmux 会话展示)。PreToolUse/PostToolUse
// 的普通遥测每工具各一次,保持不探测——hook 调用频繁,无条件探测会被
// 卡死的 tmux 服务端拖累高频工具链。$TMUX 由 tmux 服务端注入;非 tmux
// 环境为空,直接跳过。
//
// 定位分两级:首选进程树向上找控制 tty 按 list-panes 精确锚 pane——hook
// 被 Agent 以 setsid 拉起,自身无控制 tty,display-message 会回落到
// 「会话当前激活窗」(用户此刻在看哪窗就报哪窗,并非 agent 所在窗);
// Agent 进程自己持有 pane 的控制 tty,是唯一可靠锚点。无 tty 祖先时
// 回落 display-message(会话名准,窗口号 best-effort)。
func tmuxProbe(body []byte, cfg *config.Config) tmuxInfo {
	if os.Getenv("TMUX") == "" {
		return tmuxInfo{}
	}
	in, err := agent.ParseInput(body)
	if err != nil {
		return tmuxInfo{}
	}
	wantsTmuxProbe := in.HookEventName == "PermissionRequest" ||
		in.HookEventName == "Stop" ||
		(in.HookEventName == "PreToolUse" &&
			config.ApprovalMode(cfg.ApprovalMode) == config.ApprovalModePreToolUse)
	if !wantsTmuxProbe {
		return tmuxInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxProbeTimeout)
	defer cancel()
	if tty := ancestorTTY(); tty != "" {
		if info := paneByTTY(ctx, tty); info.session != "" {
			return info
		}
	}
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S #{window_index}").Output()
	if err != nil {
		return tmuxInfo{} // 失败回落:不带 header,网关按无会话处理
	}
	return parseTmuxInfo(strings.TrimSpace(string(out)))
}

// ancestorTTY 沿进程树向上找最近一个带控制 tty 的祖先,返回归一化 tty
// 名(去 /dev/ 前缀,如 ttys019)。控制 tty 不随 setsid 子进程继承——
// Agent 拉起的 hook 自身必无 tty,而 Agent 交互进程有。上限 8 层,
// ps 失败或链尽返回空。
func ancestorTTY() string {
	pid := os.Getppid()
	for i := 0; i < 8 && pid > 1; i++ {
		out, err := exec.Command("ps", "-o", "ppid=,tty=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return ""
		}
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			return ""
		}
		if tty := normalizeTTY(fields[len(fields)-1]); tty != "" {
			return tty
		}
		ppid, err := strconv.Atoi(fields[0])
		if err != nil {
			return ""
		}
		pid = ppid
	}
	return ""
}

// normalizeTTY 归一化 ps/tty 输出:去 /dev/ 前缀;无 tty 的 "?"/"??"
// 及空串返回空。
func normalizeTTY(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(s, "/dev/"))
	if s == "" || s == "?" || s == "??" {
		return ""
	}
	return s
}

// paneByTTY 在 list-panes -a 输出中按 pane tty 精确匹配,同取会话名与
// 窗口号——这是 agent 所在 pane 的真位置,与哪端正在看无关。
func paneByTTY(ctx context.Context, tty string) tmuxInfo {
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F",
		"#{pane_tty}\t#{session_name}\t#{window_index}").Output()
	if err != nil {
		return tmuxInfo{}
	}
	return parsePaneList(string(out), tty)
}

// parsePaneList 逐行匹配 pane_tty(第一个 \t 前的字段,ttys 不含空白);
// 余下段走 parseTmuxInfo(会话名可含空格,行尾 token 为窗口号)。
func parsePaneList(out, tty string) tmuxInfo {
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || normalizeTTY(parts[0]) != tty {
			continue
		}
		return parseTmuxInfo(parts[1] + " " + parts[2])
	}
	return tmuxInfo{}
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
