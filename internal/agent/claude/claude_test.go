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

// TestRenderDecisionChoice 选项作答:单问 → allow + updatedInput.answers,
// 以问题文本为键;解析不出单问时回落 deny+理由原样喂回标签。
func TestRenderDecisionChoice(t *testing.T) {
	askInput := json.RawMessage(`{"questions":[{"question":"Which database?","header":"DB","options":[{"label":"Postgres"},{"label":"SQLite"}]}]}`)
	out, err := Adapter{}.RenderDecision("PreToolUse", wire.DecisionDeny, "Postgres", "AskUserQuestion", askInput)
	if err != nil {
		t.Fatalf("RenderDecision: %v", err)
	}
	var got struct {
		HookSpecificOutput struct {
			PermissionDecision string         `json:"permissionDecision"`
			UpdatedInput       map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("单问作答必须是 allow,得 %s", out)
	}
	if _, ok := got.HookSpecificOutput.UpdatedInput["questions"]; !ok {
		t.Fatalf("updatedInput 必须保留原 questions,得 %s", out)
	}
	answers, _ := got.HookSpecificOutput.UpdatedInput["answers"].(map[string]any)
	if answers["Which database?"] != "Postgres" {
		t.Fatalf("answers 应以问题文本为键,得 %s", out)
	}

	// 解析不出单问(多问)→ 回落 deny+理由,标签仍喂回模型。
	multi := json.RawMessage(`{"questions":[{"question":"A?"},{"question":"B?"}]}`)
	fallback, _ := Adapter{}.RenderDecision("PreToolUse", wire.DecisionDeny, "X", "AskUserQuestion", multi)
	if !strings.Contains(string(fallback), `"permissionDecision":"deny"`) ||
		!strings.Contains(string(fallback), `\"X\"`) {
		t.Fatalf("多问应回落 deny 并携带标签,得 %s", fallback)
	}

	// 无 choice 的 deny 维持原话。
	plain, _ := Adapter{}.RenderDecision("PreToolUse", wire.DecisionDeny, "", "Bash", json.RawMessage(`{"command":"rm -rf build"}`))
	if !strings.Contains(string(plain), "Denied from phone") {
		t.Fatalf("无 choice deny 应维持原话,得 %s", plain)
	}
}

// TestRenderPermissionRequestUpdatedInput ExitPlanMode 的 allow 必须回显
// updatedInput(原始 tool_input),否则 Claude Code 忽略决策继续原生询问;
// allow 不带 message。
func TestRenderPermissionRequestUpdatedInput(t *testing.T) {
	plan := json.RawMessage(`{"plan":"# Plan\n- step one"}`)
	out, err := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionAllow, "", "ExitPlanMode", plan)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		HookSpecificOutput struct {
			Decision map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	d := got.HookSpecificOutput.Decision
	if d["behavior"] != "allow" {
		t.Fatalf("behavior = %v, 得 %s", d["behavior"], out)
	}
	ui, ok := d["updatedInput"].(map[string]any)
	if !ok || ui["plan"] != "# Plan\n- step one" {
		t.Fatalf("updatedInput 必须回显原 tool_input,得 %s", out)
	}
	if _, exists := d["message"]; exists {
		t.Fatalf("allow 不应带 message,得 %s", out)
	}

	// deny 维持 message,不带 updatedInput。
	deny, _ := Adapter{}.RenderDecision("PermissionRequest", wire.DecisionDeny, "", "Bash", json.RawMessage(`{"command":"ls"}`))
	var gotDeny struct {
		HookSpecificOutput struct {
			Decision map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(deny, &gotDeny); err != nil {
		t.Fatal(err)
	}
	dd := gotDeny.HookSpecificOutput.Decision
	if dd["behavior"] != "deny" || dd["message"] != "Denied from phone" {
		t.Fatalf("deny 输出不符: %s", deny)
	}
	if _, exists := dd["updatedInput"]; exists {
		t.Fatalf("deny 不应带 updatedInput,得 %s", deny)
	}
}
