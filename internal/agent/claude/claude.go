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
// answerQuestions 开启时，PreToolUse 的 AskUserQuestion 发**只通知不阻塞**：
// AskUserQuestion 不触发 PermissionRequest（Claude Code 侧它不是权限询问），
// 原生问题框就只能由终端承接。hook 立即返回（NeedsApproval=false）→ 原生
// 问题框当场出现在终端；手机卡只负责告知 + 一键导航到对应会话（用户在终端
// 自己答），不再从手机替用户作答、也不阻塞 agent 等手机决策（2026-11-11
// 拍板，取代 v0.4.x 的「手机点选项作答」）。答完后由 PostToolUse 收卡（见
// closeForQuestion）。
// 审批 ID 与 PermissionRequest 通道同源派生（session+tool+use+input），
// 若某版本 Claude Code 两通道齐发也只会登记一条。
func (Adapter) Normalize(in agent.Input, daemonID string, now int64, mode config.ApprovalMode, answerQuestions bool) (*agent.Event, error) {
	switch in.HookEventName {
	case "SessionStart":
		env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventSessionStarted, agent.BasePayload(in), now)
		return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
	case "PreToolUse":
		// 问题类永远不阻塞：优先级高于遗留的 pre-tool-use 阻塞模式——
		// 问题没有 allow/deny 语义，拦下来只会让终端里的问题框永远不出现。
		if answerQuestions && in.ToolName == "AskUserQuestion" {
			env := agent.NewApprovalEnvelope(wire.AgentClaude, daemonID, in, now)
			return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
		}
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
		// 问题答完（终端里选了）→ 用同源派生键收卡：同一 tool_use_id +
		// tool_input 必得同一 approval_id，daemon 无需维护会话关联表。
		// 关掉 answerQuestions 时没有卡可收，保持 tool_finished 遥测。
		if answerQuestions && in.ToolName == "AskUserQuestion" {
			if env, ok := closeForQuestion(in, daemonID, now); ok {
				return &agent.Event{Envelope: env, HookEvent: in.HookEventName}, nil
			}
		}
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

// closeForQuestion 为「终端里已答完」的 AskUserQuestion 构造收卡事件。
//
// approval_id 与 PreToolUse 的事件同源派生（session + tool + tool_use_id +
// 规范化 tool_input），因此 PostToolUse 不必依赖 daemon 内存里的会话关联表；
// event_id 用 "close-<id>" 与 gateway 的超时/取消收场同键，relay 侧 event_id
// 去重天然幂等。状态取 cancelled：语义是「终端侧已答」，App 收件箱据此盖
// Answered at terminal，而不是用户没做过的 Approved/Denied。
func closeForQuestion(in agent.Input, daemonID string, now int64) (wire.Envelope, bool) {
	if in.ToolUseID == "" {
		return wire.Envelope{}, false
	}
	id, err := wire.ApprovalID(in.SessionID, in.ToolName, in.ToolUseID, in.ToolInput)
	if err != nil || id == "" {
		return wire.Envelope{}, false
	}
	env := agent.NewEnvelope(wire.AgentClaude, daemonID, in, wire.EventApprovalClosed,
		agent.WithTool(agent.BasePayload(in), in), now)
	env.ApprovalID = id
	env.EventID = "close-" + id
	env.Payload.StatusText = wire.CloseReasonCancelled
	return env, true
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
