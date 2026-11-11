// Package opencode 实现 OpenCode 的插件适配器。
//
// OpenCode 没有 stdin hook 体系（不同于 Claude Code 的 settings.json /
// Codex 的 hooks.json）；接入靠 daemon 安装的全局插件文件——0.x 是
// ~/.config/opencode/plugin/devpilot-hook.ts（命名导出 +
// permission.ask），2.x 是 ~/.config/opencode/plugins/devpilot-hook.ts
// （默认导出 { id, setup } + permission.hook("evaluate")）。两代插件都把
// 事件组装成与 Claude hook 输入同构的 JSON（session_id / hook_event_name /
// tool_name / tool_input / tool_use_id / cwd），经
// `devpilot-hook hook opencode` 发到本地网关，所以本适配器直接复用
// agent.ParseInput 解析。
//
// 审批点是插件的权限钩子：插件在 OpenCode 服务端进程里阻塞等手机裁决，
// 再改写权限状态（0.x output.status / 2.x event.effect）。因此
// pre-tool-use 审批模式对 OpenCode 无效——插件无法否决已开始的工具执行，
// PreToolUse 恒作 tool_running 遥测（mode 参数被有意忽略）。网关超时/失败
// 时插件保持原生询问，行为与 Claude Code 的 hook 超时回落一致。
package opencode

import (
	"encoding/json"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Adapter 是 OpenCode 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentOpenCode }

// Normalize 把插件上报的 hook 事件映射为 wire 事件。
//
// 插件侧映射（event / permission.ask / tool.execute.*）：
//
//	session.created      → SessionStart       → session_started
//	tool.execute.before  → PreToolUse         → tool_running（节流在网关）
//	permission.ask       → PermissionRequest  → approval_required（阻塞）
//	tool.execute.after   → PostToolUse        → tool_finished（节流在网关）
//	session.idle         → Stop               → task_complete
//	session.deleted      → SessionEnd         → session_ended
//
// 插件不会发 AskUserQuestion（OpenCode 无对应工具），answerQuestions
// 参数在此无意义，保持签名一致仅作占位。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentOpenCode, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		// pre-tool-use 模式不适用于 OpenCode（插件无法否决工具执行），
		// PreToolUse 恒为遥测；审批只走 PermissionRequest。
		env := agent.NewEnvelope(wire.AgentOpenCode, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		env := agent.NewApprovalEnvelope(wire.AgentOpenCode, daemonID, in, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentOpenCode, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		// 插件当前不回传 assistant 文本，summary 恒空；字段保留与 Claude
		// 通道同构，后续插件补 last_assistant_message 时无需改协议。
		p.ReplySummary = wire.Truncate(in.LastAssistantMessage, agent.ReplySummaryRunes)
		env := agent.NewEnvelope(wire.AgentOpenCode, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentOpenCode, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成插件期望的决策 JSON（网关 200 响应体）：
//
//	{"decision":"allow"}
//	{"decision":"deny","message":"..."}
//
// 插件读到 allow/deny 后改写 permission.ask 的 output.status；其余一切
// （204 无决策、JSON 解析失败、子进程崩溃）保持 "ask"，OpenCode 弹
// 原生权限对话框。choice 是选项型审批的候选标签——OpenCode 权限无
// 问题选项，choice 恒空，但 deny+choice 的「答案喂回」语义保留与
// Claude 通道一致，防御 App 侧未来扩展。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, toolName string, toolInput json.RawMessage) ([]byte, error) {
	if d == wire.DecisionDeny {
		return json.Marshal(map[string]any{"decision": "deny", "message": denyReason(choice)})
	}
	return json.Marshal(map[string]any{"decision": "allow"})
}

// denyReason 生成 deny 的 message；选项作答时把标签原样喂回。
func denyReason(choice string) string {
	if choice == "" {
		return "Denied from phone"
	}
	return "The user has already answered this question from their phone. " +
		"Their answer is: \"" + choice + "\". Treat it as the user's final choice and proceed accordingly without asking again."
}
