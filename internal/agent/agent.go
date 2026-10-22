// Package agent 负责把各 Agent 的 hook 输入归一化为 wire 事件。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
	"github.com/twotwoba/DevPilot-Hook/internal/id"
)

// Input 是 Claude Code / Codex hook 的公共输入字段。
//
// 两个 Agent 的字段高度重合；这里保留 camelCase 别名以兼容不同版本。
type Input struct {
	SessionID            string          `json:"session_id"`
	SessionIDCamel       string          `json:"sessionId"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Source               string          `json:"source"`
	Reason               string          `json:"reason"`
	StopHookActive       bool            `json:"stop_hook_active"`
}

// ParseInput 解析 hook stdin。
func ParseInput(data []byte) (Input, error) {
	var in Input
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("agent: 解析 hook 输入失败: %w", err)
	}
	if in.SessionID == "" {
		in.SessionID = in.SessionIDCamel
	}
	if in.SessionID == "" {
		// 没有会话 ID 时用 transcript 路径派生稳定 ID，避免审批无法路由；
		// 两者都没有则拒绝，因为无法保证同一会话内幂等。
		if in.TranscriptPath == "" {
			return in, fmt.Errorf("agent: hook 输入缺少 session_id 与 transcript_path")
		}
		sum := sha256.Sum256([]byte(in.TranscriptPath))
		in.SessionID = "sess_" + hex.EncodeToString(sum[:])[:16]
	}
	return in, nil
}

// Event 是归一化结果。
type Event struct {
	Envelope      wire.Envelope
	HookEvent     string
	NeedsApproval bool
}

// Adapter 隔离 Agent 之间的差异。
type Adapter interface {
	Name() wire.Agent
	// Normalize 返回 nil 表示该 hook 事件不需要上报。
	Normalize(in Input, daemonID string, now int64, mode config.ApprovalMode) (*Event, error)
	// RenderDecision 生成 hook stdout；只应在拿到 allow/deny 时调用。
	RenderDecision(hookEvent string, d wire.DecisionKind) ([]byte, error)
}

// BasePayload 生成所有事件共有的摘要字段。
func BasePayload(in Input) wire.Payload {
	title := "Agent 会话"
	if in.Cwd != "" {
		if base := filepath.Base(in.Cwd); base != "." && base != string(filepath.Separator) {
			title = base
		}
	}
	title = wire.Truncate(title, wire.MaxTitleRunes)
	p := wire.Payload{Title: title, SessionTitle: title}
	if in.Prompt != "" {
		p.PromptSummary = wire.Summarize(in.Prompt, wire.MaxPromptRunes)
	}
	return p
}

// WithTool 补充工具调用摘要（不含完整输入）。
func WithTool(p wire.Payload, in Input) wire.Payload {
	p.ToolName = wire.Truncate(in.ToolName, 64)
	p.CommandSummary = wire.SummarizeToolInput(in.ToolInput)
	p.InputDigest = wire.InputDigest(in.ToolInput)
	return p
}

// NewEnvelope 构造事件包封。
func NewEnvelope(a wire.Agent, daemonID string, in Input, t wire.EventType, p wire.Payload, now int64) wire.Envelope {
	return wire.Envelope{
		V:         wire.Version,
		Kind:      wire.KindEvent,
		MsgID:     id.New(),
		EventID:   id.New(),
		Type:      t,
		Agent:     a,
		DaemonID:  daemonID,
		SessionID: in.SessionID,
		Payload:   p,
		TS:        now,
	}
}

// NewApprovalEnvelope 构造审批事件；tool_use_id 缺失时退化为随机 ID。
func NewApprovalEnvelope(a wire.Agent, daemonID string, in Input, now int64) wire.Envelope {
	env := NewEnvelope(a, daemonID, in, wire.EventApprovalRequired, WithTool(BasePayload(in), in), now)
	approvalID := ""
	if in.ToolUseID != "" {
		if v, err := wire.ApprovalID(in.SessionID, in.ToolName, in.ToolUseID, in.ToolInput); err == nil {
			approvalID = v
		}
	}
	if approvalID == "" {
		approvalID = "apv_" + id.New()
	}
	env.ApprovalID = approvalID
	return env
}
