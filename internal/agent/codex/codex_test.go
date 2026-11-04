package codex

import (
	"strings"
	"testing"

	"github.com/twotwoba/DevPilot-Hook/internal/agent"
	"github.com/twotwoba/DevPilot-Hook/internal/config"
)

// Stop 回复摘要走详情帽（agent.ReplySummaryRunes=2000）且保结构,
// 与 claude adapter 同口径;超帽截断带省略号。
func TestStopReplySummaryDetailCapKeepsNewlines(t *testing.T) {
	in := agent.Input{
		SessionID:            "s1",
		HookEventName:        "Stop",
		Cwd:                  "/tmp/proj",
		LastAssistantMessage: "结论如下：\n- 第一点\n- 第二点",
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
