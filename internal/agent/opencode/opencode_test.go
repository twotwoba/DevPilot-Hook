package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

func opencodeInput(event string) agent.Input {
	return agent.Input{
		SessionID:     "ses_opencode_1",
		HookEventName: event,
		Cwd:           "/Users/eric/Desktop/DevPilot",
		ToolName:      "bash",
		ToolUseID:     "call_1",
		ToolInput:     json.RawMessage(`{"command":"git push","patterns":["git push *"]}`),
	}
}

// TestNormalizeEventMapping 钉住插件六类事件的 wire 映射与审批旗标。
func TestNormalizeEventMapping(t *testing.T) {
	a := Adapter{}
	cases := []struct {
		event     string
		want      wire.EventType
		approving bool
	}{
		{"SessionStart", wire.EventSessionStarted, false},
		{"PreToolUse", wire.EventToolRunning, false},
		{"PermissionRequest", wire.EventApprovalRequired, true},
		{"PostToolUse", wire.EventToolFinished, false},
		{"Stop", wire.EventTaskComplete, false},
		{"SessionEnd", wire.EventSessionEnded, false},
	}
	for _, c := range cases {
		ev, err := a.Normalize(opencodeInput(c.event), "dd_1", 1000, config.ApprovalModePermissionRequest, true)
		if err != nil {
			t.Fatalf("%s: %v", c.event, err)
		}
		if ev == nil {
			t.Fatalf("%s: 不应返回 nil", c.event)
		}
		if ev.Envelope.Type != c.want {
			t.Fatalf("%s: 事件类型 = %s, 期望 %s", c.event, ev.Envelope.Type, c.want)
		}
		if ev.NeedsApproval != c.approving {
			t.Fatalf("%s: NeedsApproval = %v, 期望 %v", c.event, ev.NeedsApproval, c.approving)
		}
		if ev.Envelope.Agent != wire.AgentOpenCode {
			t.Fatalf("%s: agent = %s, 期望 opencode", c.event, ev.Envelope.Agent)
		}
	}
}

// TestNormalizeUnknownEventDropped 未识别的事件不产生云端事件。
func TestNormalizeUnknownEventDropped(t *testing.T) {
	ev, err := Adapter{}.Normalize(opencodeInput("Notification"), "dd_1", 1000, config.ApprovalModePermissionRequest, true)
	if err != nil || ev != nil {
		t.Fatalf("未知事件应返回 nil,nil,实际 %v,%v", ev, err)
	}
}

// TestNormalizePreToolUseNeverApproves pre-tool-use 审批模式对 OpenCode
// 无效（插件无法否决工具执行）,PreToolUse 在任何模式下都是遥测。
func TestNormalizePreToolUseNeverApproves(t *testing.T) {
	for _, mode := range []config.ApprovalMode{config.ApprovalModePermissionRequest, config.ApprovalModePreToolUse} {
		ev, err := Adapter{}.Normalize(opencodeInput("PreToolUse"), "dd_1", 1000, mode, true)
		if err != nil {
			t.Fatal(err)
		}
		if ev.NeedsApproval {
			t.Fatalf("模式 %s 下 PreToolUse 不应升格为审批", mode)
		}
	}
}

// TestNormalizeApprovalEnvelope 审批事件带确定性 approval_id 与完整摘要字段。
func TestNormalizeApprovalEnvelope(t *testing.T) {
	ev, err := Adapter{}.Normalize(opencodeInput("PermissionRequest"), "dd_1", 1000, config.ApprovalModePermissionRequest, true)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Envelope.ApprovalID == "" {
		t.Fatal("审批事件缺少 approval_id")
	}
	if ev.Envelope.Payload.ToolName != "bash" {
		t.Fatalf("tool_name = %q", ev.Envelope.Payload.ToolName)
	}
	if ev.Envelope.Payload.CommandSummary == "" {
		t.Fatal("command_summary 不应为空(审批卡正文数据源)")
	}
	if err := ev.Envelope.Validate(); err != nil {
		t.Fatalf("信封应过 wire 校验: %v", err)
	}
}

// TestRenderDecisionAllowAllowDecisionShape 决策 JSON 是插件消费的
// 简单契约：allow 不带 message,deny 带人话理由。
func TestRenderDecisionAllowDecisionShape(t *testing.T) {
	out, err := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionAllow, "", "bash", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["decision"] != "allow" {
		t.Fatalf("decision = %v", m["decision"])
	}
	if _, ok := m["message"]; ok {
		t.Fatal("allow 不应携带 message")
	}
}

func TestRenderDecisionDenyDecisionShape(t *testing.T) {
	out, err := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionDeny, "", "bash", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["decision"] != "deny" {
		t.Fatalf("decision = %v", m["decision"])
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "Denied from phone") {
		t.Fatalf("deny message = %q", m["message"])
	}
}

// TestRenderDecisionDenyWithChoice deny+choice 把答案喂回(防御保留语义)。
func TestRenderDecisionDenyWithChoice(t *testing.T) {
	out, err := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionDeny, "Use rebase", "bash", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if msg, _ := m["message"].(string); !strings.Contains(msg, "Use rebase") {
		t.Fatalf("deny message 应携带选项文本,实际 %q", m["message"])
	}
}
