// Package claude 实现 Claude Code 的 hook 适配器。
package claude

import (
	"encoding/json"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Adapter 是 Claude Code 适配器。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentClaude }

// Normalize 把 Claude Code hook 事件映射为 wire 事件。
//
// 默认用 PermissionRequest 作为审批主通道：它只在 Claude Code 真的要弹
// 原生权限询问时触发。PreToolUse 仅用于 tool_running 遥测；旧版本可用
// pre-tool-use 模式让 PreToolUse 直接阻塞审批。
//
// answerQuestions 开启时，PreToolUse 的 AskUserQuestion 升格为审批：
// AskUserQuestion 不触发 PermissionRequest（Claude Code 侧它不是权限
// 询问），原生链路只能在终端等答案；拦下来手机卡片直接点选项作答。
// 审批 ID 与 PermissionRequest 通道同源派生（session+tool+use+input），
// 若某版本 Claude Code 两通道齐发也只会登记一条。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		if mode == config.ApprovalModePreToolUse ||
			(answerQuestions && in.ToolName == "AskUserQuestion") {
			env := agent.NewApprovalEnvelope(wire.AgentClaude, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		env := agent.NewApprovalEnvelope(wire.AgentClaude, daemonID, in, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		// 详情帽+保结构(见 agent.ReplySummaryRunes):换行不压扁,markdown 可渲染
		p.ReplySummary = wire.Truncate(in.LastAssistantMessage, agent.ReplySummaryRunes)
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		// 未识别的 hook（如 Notification / UserPromptSubmit）不产生云端事件。
		return nil, nil
	}
}

// RenderDecision 生成 Claude Code 期望的 hook 输出。
//
// choice 非空 = 选项作答：deny + 理由携带所选标签喂回模型——Claude Code
// 对 PreToolUse deny 的 reason 就是模型的输入，模型把它当用户的最终选择
// 继续推进，不再重复追问。这是文档化行为，不赌 ask 输出 schema。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice string) ([]byte, error) {
	behavior := "allow"
	message := "Approved from phone"
	if d == wire.DecisionDeny {
		behavior = "deny"
		message = "Denied from phone"
		if choice != "" {
			message = "The user has already answered this question from their phone. " +
				"Their answer is: \"" + choice + "\". Treat it as the user's final choice and proceed accordingly without asking again."
		}
	}

	if hookEvent == "PreToolUse" {
		return json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       behavior,
				"permissionDecisionReason": message,
			},
		})
	}
	return json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision": map[string]any{
				"behavior": behavior,
				"message":  message,
			},
		},
	})
}
