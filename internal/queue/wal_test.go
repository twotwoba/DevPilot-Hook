package queue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

func env(id string) wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_" + id, EventID: id,
		Type: wire.EventToolRunning, Agent: wire.AgentClaude,
		DaemonID: "dd_x", SessionID: "sess_x", TS: 1,
	}
}

func TestPutReplayAndAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.wal")
	w, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := w.Put(env(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Put(env("e1")); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 3 {
		t.Fatalf("重复 Put 应幂等: %d", w.Len())
	}
	if err := w.Ack("e2"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 重放后顺序应保持。
	w2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	pending := w2.Pending()
	if len(pending) != 2 || pending[0].EventID != "e1" || pending[1].EventID != "e3" {
		t.Fatalf("重放结果不符: %+v", pending)
	}
}

func TestTruncatedTailRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.wal")
	w, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Put(env("e1")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟崩溃：追加半行 JSON。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"op":"put","event_id":"broken"`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	w2, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("末行截断应可恢复: %v", err)
	}
	if w2.Len() != 1 {
		t.Fatalf("应保留完整行: %d", w2.Len())
	}
	// 追加新事件后重新打开，确认没有和半行拼接。
	if err := w2.Put(env("e2")); err != nil {
		t.Fatal(err)
	}
	_ = w2.Close()

	w3, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("恢复后再次打开失败: %v", err)
	}
	defer w3.Close()
	if w3.Len() != 2 {
		t.Fatalf("恢复后事件数不符: %d", w3.Len())
	}
}

func TestCompactionKeepsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.wal")
	w, err := Open(path, Options{MaxBytes: 1, MaxRecords: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := w.Put(env(id)); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Ack("e1")
	_ = w.Ack("e3")
	size := w.Size()
	_ = w.Close()

	w2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	pending := w2.Pending()
	if len(pending) != 1 || pending[0].EventID != "e2" {
		t.Fatalf("压实后 pending 不符: %+v", pending)
	}
	if size > 4096 {
		t.Fatalf("压实后仍然过大: %d", size)
	}
}

func TestCorruptMiddleRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.wal")
	data := "{\"op\":\"put\",\"event_id\":\"e1\",\"envelope\":{\"event_id\":\"e1\"}}\nnot-json\n{\"op\":\"ack\",\"event_id\":\"e1\"}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("中段损坏必须拒绝启动")
	}
}
