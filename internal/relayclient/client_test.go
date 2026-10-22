package relayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
	"github.com/twotwoba/DevPilot-Hook/internal/event"
	"github.com/twotwoba/DevPilot-Hook/internal/queue"
)

const credential = "dpd_dd_test_secret"

func openWAL(t *testing.T) *queue.WAL {
	t.Helper()
	w, err := queue.Open(filepath.Join(t.TempDir(), "queue.wal"), queue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func approvalEnvelope() wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m1", EventID: "e1",
		ApprovalID: "apv_test", Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: "dd_test", SessionID: "sess_test", TS: 100,
	}
}

func readMsg(t *testing.T, ctx context.Context, conn *websocket.Conn) any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Errorf("读取消息失败: %v", err)
		return nil
	}
	msg, err := wire.Decode(data)
	if err != nil {
		t.Errorf("解码消息失败: %v (%s)", err, data)
		return nil
	}
	return msg
}

func TestHelloEventDecisionAck(t *testing.T) {
	wal := openWAL(t)
	env := approvalEnvelope()
	if err := wal.Put(env); err != nil {
		t.Fatal(err)
	}
	reg := event.NewRegistry()
	reg.Add(env, time.Now(), time.Minute)

	ackCh := make(chan wire.Ack, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		hello, ok := readMsg(t, ctx, conn).(*wire.Hello)
		if !ok || hello.DaemonID != "dd_test" || hello.Role != wire.RoleDaemon {
			t.Errorf("hello 不符: %+v", hello)
			return
		}
		// 服务端应先收到 WAL 里的待发事件。
		got, ok := readMsg(t, ctx, conn).(*wire.Envelope)
		if !ok || got.EventID != "e1" {
			t.Errorf("事件不符: %+v", got)
			return
		}
		decision := wire.Decision{
			V: wire.Version, Kind: wire.KindDecision, MsgID: "d1",
			ApprovalID: "apv_test", Decision: wire.DecisionAllow, TS: 101,
		}
		data, _ := json.Marshal(decision)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			return
		}
		ack, ok := readMsg(t, ctx, conn).(*wire.Ack)
		if !ok || ack.EventID != "" {
			t.Errorf("ack 不符: %+v", ack)
			return
		}
		ackCh <- *ack
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	client, err := New(Config{
		RelayURL:     srv.URL,
		Credential:   credential,
		DaemonID:     "dd_test",
		WAL:          wal,
		Registry:     reg,
		PingInterval: time.Hour,
		BackoffMin:   10 * time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	select {
	case ack := <-ackCh:
		if ack.Status != wire.AckAccepted || ack.ApprovalID != "apv_test" {
			t.Fatalf("决策 ack 不符: %+v", ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待决策 ack 超时")
	}

	waitFor(t, 3*time.Second, func() bool { return reg.Len() > 0 })
	res := reg.Wait(context.Background(), "apv_test")
	if res.Status != event.StatusApproved || res.Decision != wire.DecisionAllow {
		t.Fatalf("审批状态不符: %+v", res)
	}
}

func TestReconnectResendsUnacked(t *testing.T) {
	wal := openWAL(t)
	if err := wal.Put(approvalEnvelope()); err != nil {
		t.Fatal(err)
	}
	reg := event.NewRegistry()

	var connections atomic.Int32
	var mu sync.Mutex
	var received []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connections.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		if _, ok := readMsg(t, ctx, conn).(*wire.Hello); !ok {
			return
		}
		msg := readMsg(t, ctx, conn)
		env, ok := msg.(*wire.Envelope)
		if !ok {
			t.Errorf("连接 %d 未收到事件: %+v", n, msg)
			return
		}
		mu.Lock()
		received = append(received, env.EventID)
		mu.Unlock()

		if n == 1 {
			// 第一连接直接断开且不 ack，模拟网络闪断。
			_ = conn.Close(websocket.StatusAbnormalClosure, "drop")
			return
		}
		ack := wire.Ack{
			V: wire.Version, Kind: wire.KindAck, MsgID: "a1",
			EventID: env.EventID, Status: wire.AckAccepted, TS: 102,
		}
		data, _ := json.Marshal(ack)
		_ = conn.Write(ctx, websocket.MessageText, data)
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	client, err := New(Config{
		RelayURL:     srv.URL,
		Credential:   credential,
		DaemonID:     "dd_test",
		WAL:          wal,
		Registry:     reg,
		PingInterval: time.Hour,
		BackoffMin:   10 * time.Millisecond,
		BackoffMax:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return wal.Len() == 0 && connections.Load() >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 || received[0] != "e1" || received[1] != "e1" {
		t.Fatalf("重连后应重发同一事件: %v", received)
	}
}

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"https://relay.example.com":       "wss://relay.example.com/ws/daemon",
		"http://127.0.0.1:8080":           "ws://127.0.0.1:8080/ws/daemon",
		"https://relay.example.com/base/": "wss://relay.example.com/base/ws/daemon",
	}
	for in, want := range cases {
		got, err := wsURL(in)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s -> %s，期望 %s", in, got, want)
		}
	}
	if _, err := wsURL("ftp://x"); err == nil {
		t.Fatal("不支持的 scheme 应报错")
	}
}

func TestJitterBounds(t *testing.T) {
	base := time.Second
	for range 100 {
		got := jitter(base)
		if got < 800*time.Millisecond || got >= 1200*time.Millisecond {
			t.Fatalf("抖动越界: %v", got)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("缺配置应报错: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}
