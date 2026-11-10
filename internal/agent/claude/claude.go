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
// 2026-11 协议修正（真机取证：ExitPlanMode 点 Approve 后终端纹丝不动、
// 23 秒后落 source=user_temporary）：
//   - PermissionRequest 的 allow 必须回显 updatedInput（原始 tool_input），
//     只给 behavior 会被忽略、原生审批框继续停留；allow 不带 message
//     （文档里 message 只给 deny 用）。
//   - PreToolUse + AskUserQuestion + choice 走 allow + updatedInput.answers：
//     问题当场得到答案，模型不再重复追问；解析不出单问时回落旧路
//     （deny + 理由携带标签，模型仍能读到答案，能力不丢）。
func (Adapter) RenderDecision(hookEvent string, d wire.DecisionKind, choice, toolName string, toolInput json.RawMessage) ([]byte, error) {
	if hookEvent == "PreToolUse" {
		if choice != "" && toolName == "AskUserQuestion" {
			if out, ok := askAnswer(toolInput, choice); ok {
				return json.Marshal(out)
			}
		}
		if d == wire.DecisionAllow {
			output := map[string]any{
				"hookEventName":      "PreToolUse",
				"permissionDecision": "allow",
			}
			if obj, ok := decodeObject(toolInput); ok &&
				(toolName == "ExitPlanMode" || toolName == "AskUserQuestion") {
				output["updatedInput"] = obj
			}
			return json.Marshal(map[string]any{"hookSpecificOutput": output})
		}
		return json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       "deny",
				"permissionDecisionReason": denyReason(choice),
			},
		})
	}

	decision := map[string]any{"behavior": "allow"}
	if d == wire.DecisionDeny {
		decision["behavior"] = "deny"
		decision["message"] = denyReason(choice)
	} else if obj, ok := decodeObject(toolInput); ok {
		decision["updatedInput"] = obj
	}
	return json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PermissionRequest",
			"decision":      decision,
		},
	})
}

// denyReason 生成 deny 的 message；选项作答时把标签原样喂回模型。
func denyReason(choice string) string {
	if choice == "" {
		return "Denied from phone"
	}
	return "The user has already answered this question from their phone. " +
		"Their answer is: \"" + choice + "\". Treat it as the user's final choice and proceed accordingly without asking again."
}

// decodeObject 把原始 tool_input 解析成对象；空/非法/非对象一律 false。
func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// askAnswer 构造 AskUserQuestion 的作答输出：原 questions + answers 映射。
// answers 以问题文本为键（Claude Code 的多问协议），只处理恰好一问——
// 多问的选项 App 侧不会下发（ExtractOptions 已挡），这里防御回落。
func askAnswer(toolInput json.RawMessage, choice string) (map[string]any, bool) {
	obj, ok := decodeObject(toolInput)
	if !ok {
		return nil, false
	}
	qs, ok := obj["questions"].([]any)
	if !ok || len(qs) != 1 {
		return nil, false
	}
	q, ok := qs[0].(map[string]any)
	if !ok {
		return nil, false
	}
	question, _ := q["question"].(string)
	if question == "" {
		return nil, false
	}
	obj["answers"] = map[string]any{question: choice}
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "allow",
			"updatedInput":       obj,
		},
	}, true
}
