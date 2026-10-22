// Package codex 实现 Codex CLI 的 hook 适配器。
package codex

import (
	"encoding/json"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Adapter 是 Codex 适配器。
//
// Codex 的 hook 字段与 Claude Code 高度同构，但只支持 allow/deny，
// 没有 ask；因此审批结果始终编码为 permissionDecision。
type Adapter struct{}

// Name 返回 wire 中的 agent 标识。
func (Adapter) Name() wire.Agent { return wire.AgentCodex }

// Normalize 把 Codex hook 事件映射为 wire 事件。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentCodex, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		if mode == config.ApprovalModePreToolUse {
			env := agent.NewApprovalEnvelope(wire.AgentCodex, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
		}
		env := agent.NewEnvelope(wire.AgentCodex, daemonID, in, wire.EventToolRunning, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PermissionRequest":
		env := agent.NewApprovalEnvelope(wire.AgentCodex, daemonID, in, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName, NeedsApproval: true}, nil
	case "PostToolUse":
		env := agent.NewEnvelope(wire.AgentCodex, daemonID, in, wire.EventToolFinished, agent.WithTool(agent.BasePayload(in), in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "Stop":
		p := agent.BasePayload(in)
		p.ReplySummary = wire.Summarize(in.LastAssistantMessage, wire.MaxReplyRunes)
		env := agent.NewEnvelope(wire.AgentCodex, daemonID, in, wire.EventTaskComplete, p, now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "SessionEnd":
		env := agent.NewEnvelope(wire.AgentCodex, daemonID, in, wire.EventSessionEnded, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	default:
		return nil, nil
	}
}

// RenderDecision 生成 Codex 期望的 hook 输出。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind) ([]byte, error) {
	decision := "allow"
	reason := "Approved from phone"
	if d == wire.DecisionDeny {
		decision = "deny"
		reason = "Denied from phone"
	}
	event := hookEvent
	if event == "" || event == "PermissionRequest" {
		event = "PreToolUse"
	}
	return json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            event,
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	})
}
