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
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		if mode == config.ApprovalModePreToolUse {
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
		p.ReplySummary = wire.Summarize(in.LastAssistantMessage, wire.MaxReplyRunes)
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
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind) ([]byte, error) {
	behavior := "allow"
	message := "Approved from phone"
	if d == wire.DecisionDeny {
		behavior = "deny"
		message = "Denied from phone"
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
