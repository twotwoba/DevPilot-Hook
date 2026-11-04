package claude

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Stop 回复摘要走详情帽（agent.ReplySummaryRunes=2000）且保结构:
// 未超帽原文透传（换行保留,markdown 栅栏/列表在 App 端可渲染）,超帽截断
// 带省略号。横幅 80 帽的口径由 relay 侧组包自理,不经本 adapter。
func TestStopReplySummaryDetailCapKeepsNewlines(t *testing.T) {
	// 未超帽:原文透传,换行不压扁
	in := agent.Input{
		SessionID:            "s1",
		HookEventName:        "Stop",
		Cwd:                  "/tmp/proj",
		LastAssistantMessage: "结论如下：\n- 第一点\n- 第二点\n```go\ncode()\n```",
	}
	ev, err := Adapter{}.Normalize(in, "dv_1", 1_000, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if ev.Envelope.Type != "task_complete" {
		t.Fatalf("Stop 应映射 task_complete,得 %q", ev.Envelope.Type)
	}
	if got := ev.Envelope.Payload.ReplySummary; got != in.LastAssistantMessage {
		t.Fatalf("未超帽应原文透传(保换行),得 %q", got)
	}

	// 超帽:截到 2000 runes,末尾省略号
	long := strings.Repeat("汉", 3000)
	ev2, err := Adapter{}.Normalize(agent.Input{
		SessionID:            "s2",
		HookEventName:        "Stop",
		LastAssistantMessage: long,
	}, "dv_1", 1_000, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatalf("Normalize(long): %v", err)
	}
	got2 := ev2.Envelope.Payload.ReplySummary
	if n := len([]rune(got2)); n != agent.ReplySummaryRunes {
		t.Fatalf("超帽应截到 %d runes,得 %d", agent.ReplySummaryRunes, n)
	}
	if !strings.HasSuffix(got2, "…") {
		t.Fatalf("截断应以省略号收尾,尾部得 %q", got2[len(got2)-3:])
	}
}

// askIn 构造 PreToolUse 的 AskUserQuestion hook 输入（单问两选项）。
func askIn() agent.Input {
	return agent.Input{
		SessionID:     "s1",
		HookEventName: "PreToolUse",
		Cwd:           "/tmp/proj",
		ToolName:      "AskUserQuestion",
		ToolUseID:     "tu_1",
		ToolInput:     json.RawMessage(`{"questions":[{"question":"Which auth?","header":"Auth","options":[{"label":"JWT"},{"label":"Session cookie"}]}]}`),
	}
}

// TestAskUserQuestionInterception 选项作答拦截:AskUserQuestion 不触发
// PermissionRequest,PreToolUse 是唯一入口;开关开→审批+Options 随卡,
// 开关关→tool_running 遥测(行为与旧版完全一致)。
func TestAskUserQuestionInterception(t *testing.T) {
	// 开:升格审批,payload 带选项标签
	ev, err := Adapter{}.Normalize(askIn(), "dv_1", 1_000, config.ApprovalModePermissionRequest, true)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !ev.NeedsApproval || ev.Envelope.Type != "approval_required" {
		t.Fatalf("answerQuestions=true 应升格审批,得 type=%q needsApproval=%v", ev.Envelope.Type, ev.NeedsApproval)
	}
	want := []string{"JWT", "Session cookie"}
	if !reflect.DeepEqual(ev.Envelope.Payload.Options, want) {
		t.Fatalf("Options 应原样带序,得 %#v", ev.Envelope.Payload.Options)
	}
	if ev.Envelope.ApprovalID == "" {
		t.Fatal("审批事件必须带 approval_id")
	}

	// 关:遥测,行为与旧版一致,不产卡
	ev2, err := Adapter{}.Normalize(askIn(), "dv_1", 1_000, config.ApprovalModePermissionRequest, false)
	if err != nil {
		t.Fatalf("Normalize(off): %v", err)
	}
	if ev2.NeedsApproval || ev2.Envelope.Type != "tool_running" {
		t.Fatalf("answerQuestions=false 应保持遥测,得 type=%q", ev2.Envelope.Type)
	}

	// 普通工具不受开关影响(PermissionRequest 仍是主审批通道)
	bash := agent.Input{SessionID: "s1", HookEventName: "PreToolUse", ToolName: "Bash", ToolUseID: "tu_2", ToolInput: json.RawMessage(`{"command":"ls"}`)}
	ev3, err := Adapter{}.Normalize(bash, "dv_1", 1_000, config.ApprovalModePermissionRequest, true)
	if err != nil {
		t.Fatalf("Normalize(bash): %v", err)
	}
	if ev3.NeedsApproval || ev3.Envelope.Type != "tool_running" {
		t.Fatalf("普通工具不应被拦截,得 type=%q", ev3.Envelope.Type)
	}
}

// TestRenderDecisionChoice 选项作答的回喂文本:deny+choice 的 reason 就是
// 模型的输入,答案必须原样在文本里;无 choice 的 deny 维持原话。
func TestRenderDecisionChoice(t *testing.T) {
	out, err := Adapter{}.RenderDecision("PreToolUse", wire.DecisionDeny, "Session cookie")
	if err != nil {
		t.Fatalf("RenderDecision: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"permissionDecision":"deny"`) {
		t.Fatalf("选项作答必须是 deny(文档化行为),得 %s", s)
	}
	if !strings.Contains(s, "Session cookie") {
		t.Fatalf("reason 必须携带所选标签,得 %s", s)
	}

	// 无 choice 的 deny 维持原话;allow 不受 choice 影响
	plain, _ := Adapter{}.RenderDecision("PreToolUse", wire.DecisionDeny, "")
	if !strings.Contains(string(plain), "Denied from phone") {
		t.Fatalf("无 choice deny 应维持原话,得 %s", plain)
	}
	allow, _ := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionAllow, "")
	if !strings.Contains(string(allow), `"behavior":"allow"`) {
		t.Fatalf("allow 输出形状不变,得 %s", allow)
	}
}
