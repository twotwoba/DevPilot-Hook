package event

import (
	"context"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

func testEnvelope(id string) wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_" + id, EventID: "e_" + id,
		ApprovalID: id, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: "dd_x", SessionID: "sess_x", TS: 1,
	}
}

func TestFirstDecisionWins(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	if _, isNew := r.Add(testEnvelope("apv_1"), now, time.Minute); !isNew {
		t.Fatal("首次登记应为新")
	}
	if _, isNew := r.Add(testEnvelope("apv_1"), now, time.Minute); isNew {
		t.Fatal("重复登记应返回已有审批")
	}

	done := make(chan Result, 1)
	go func() { done <- r.Wait(context.Background(), "apv_1") }()

	res := r.Decide("apv_1", wire.DecisionAllow, now)
	if !res.Accepted || res.Status != StatusApproved {
		t.Fatalf("首次决策应生效: %+v", res)
	}
	got := <-done
	if got.Status != StatusApproved || got.Decision != wire.DecisionAllow {
		t.Fatalf("Wait 结果不符: %+v", got)
	}

	dup := r.Decide("apv_1", wire.DecisionDeny, now)
	if dup.Accepted || dup.Status != StatusApproved || dup.Decision != wire.DecisionAllow {
		t.Fatalf("重复决策必须返回第一次结果: %+v", dup)
	}
}

func TestWaitTimeoutExpires(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(testEnvelope("apv_2"), now, 30*time.Millisecond)

	res := r.Wait(context.Background(), "apv_2")
	if res.Status != StatusExpired {
		t.Fatalf("期望 expired，实际 %+v", res)
	}
	// 过期后的决策不能再放行。
	late := r.Decide("apv_2", wire.DecisionAllow, now.Add(time.Second))
	if late.Accepted || late.Status != StatusExpired {
		t.Fatalf("过期后决策必须被拒: %+v", late)
	}
}

func TestWaitContextCancel(t *testing.T) {
	r := NewRegistry()
	r.Add(testEnvelope("apv_3"), time.Now(), time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- r.Wait(ctx, "apv_3") }()
	time.Sleep(10 * time.Millisecond)
	cancel()

	res := <-done
	if res.Status != StatusCancelled {
		t.Fatalf("期望 cancelled，实际 %+v", res)
	}
	if r.Decide("apv_3", wire.DecisionAllow, time.Now()).Accepted {
		t.Fatal("取消后不能再决策")
	}
}

func TestUnknownApproval(t *testing.T) {
	r := NewRegistry()
	if res := r.Wait(context.Background(), "apv_missing"); res.Status != StatusUnknown {
		t.Fatalf("未知审批应返回 unknown: %+v", res)
	}
	if res := r.Decide("apv_missing", wire.DecisionAllow, time.Now()); res.Status != StatusUnknown {
		t.Fatalf("未知审批决策应返回 unknown: %+v", res)
	}
}

func TestExpireAndCleanup(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	r.Add(testEnvelope("apv_4"), now, 10*time.Millisecond)
	r.Add(testEnvelope("apv_5"), now, time.Hour)

	expired := r.Expire(now.Add(time.Second))
	if len(expired) != 1 || expired[0].ID != "apv_4" {
		t.Fatalf("过期列表不符: %+v", expired)
	}
	if got := r.Pending(); len(got) != 1 || got[0].ApprovalID != "apv_5" {
		t.Fatalf("pending 快照不符: %+v", got)
	}
	if n := r.Cleanup(now.Add(2*time.Hour), time.Minute); n != 1 {
		t.Fatalf("清理数量不符: %d", n)
	}
}

func TestThrottleFixedWindow(t *testing.T) {
	th := NewThrottle(time.Minute)
	now := time.Unix(1000, 0)
	if !th.Allow("s:tool_running", now) {
		t.Fatal("首次应放行")
	}
	if th.Allow("s:tool_running", now.Add(30*time.Second)) {
		t.Fatal("窗口内应丢弃")
	}
	if !th.Allow("s:tool_finished", now) {
		t.Fatal("不同事件类型互不影响")
	}
	if !th.Allow("s:tool_running", now.Add(time.Minute)) {
		t.Fatal("窗口过后应放行")
	}

	th.Forget("s:")
	if th.Len() != 0 {
		t.Fatalf("Forget 后应为空: %d", th.Len())
	}
}

func TestThrottleZeroAlwaysAllows(t *testing.T) {
	th := NewThrottle(0)
	for range 3 {
		if !th.Allow("k", time.Now()) {
			t.Fatal("interval=0 应全部放行")
		}
	}
}
