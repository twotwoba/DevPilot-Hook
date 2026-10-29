// Package queue 实现 append-only JSONL 离线队列。
package queue

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// 默认压实阈值。
const (
	DefaultMaxRecords = 10000
	DefaultMaxBytes   = 16 << 20 // 16MB
)

// Options 控制压实阈值。
type Options struct {
	MaxRecords int
	MaxBytes   int64
}

// record 是 WAL 的一行。
type record struct {
	Op         string         `json:"op"`
	EventID    string         `json:"event_id"`
	Envelope   *wire.Envelope `json:"envelope,omitempty"`
	EnqueuedAt int64          `json:"enqueued_at,omitempty"`
}

// WAL 是 daemon 的离线事件队列。
//
// 事件先落盘再上行；relay ack 后写一条 ack 记录，压实的时候被丢弃。
type WAL struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	opts    Options
	pending map[string]wire.Envelope
	order   []string
	size    int64
}

// Open 打开（或创建）WAL 并重放。
func Open(path string, opts Options) (*WAL, error) {
	if opts.MaxRecords <= 0 {
		opts.MaxRecords = DefaultMaxRecords
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	w := &WAL{
		path:    path,
		opts:    opts,
		pending: make(map[string]wire.Envelope),
	}
	if err := w.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("queue: failed to open WAL: %w", err)
	}
	w.file = f
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.size = info.Size()
	return w, nil
}

// replay 读取历史记录；只容忍末行被截断。
func (w *WAL) replay() error {
	f, err := os.Open(w.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("queue: failed to read WAL: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)

	var offset int64
	var goodOffset int64
	lineNo := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		lineNo++
		offset += int64(len(line)) + 1 // ScanLines 去掉了 \n

		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			// 判断是否末行：再读一行试试。
			if !scanner.Scan() {
				// 末行损坏（崩溃时写了一半），截断到上一个完整行。
				if err := os.Truncate(w.path, goodOffset); err != nil {
					return fmt.Errorf("queue: failed to truncate corrupted tail: %w", err)
				}
				return nil
			}
			// 不是末行：数据真的损坏了，拒绝启动以免静默丢事件。
			return fmt.Errorf("queue: corrupted WAL line %d: %w", lineNo, err)
		}
		goodOffset = offset

		switch rec.Op {
		case "put":
			if rec.Envelope == nil || rec.EventID == "" {
				continue
			}
			if _, ok := w.pending[rec.EventID]; !ok {
				w.pending[rec.EventID] = *rec.Envelope
				w.order = append(w.order, rec.EventID)
			}
		case "ack":
			delete(w.pending, rec.EventID)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("queue: failed to scan WAL: %w", err)
	}

	// 崩溃可能留下一个完整但无换行结尾的 JSON；补上换行，
	// 否则下一次 append 会与它拼成一行。
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		if _, err := f.Seek(-1, 2); err != nil { // 2 = io.SeekEnd
			return err
		}
		var last [1]byte
		if _, err := f.Read(last[:]); err != nil {
			return err
		}
		if last[0] != '\n' {
			af, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			if _, err := af.Write([]byte{'\n'}); err != nil {
				_ = af.Close()
				return err
			}
			if err := af.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Put 追加一条事件；重复 event_id 幂等。
func (w *WAL) Put(env wire.Envelope) error {
	if env.EventID == "" {
		return fmt.Errorf("queue: event missing event_id")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[env.EventID]; ok {
		return nil
	}
	rec := record{Op: "put", EventID: env.EventID, Envelope: &env, EnqueuedAt: wire.NowUnix()}
	if err := w.appendLocked(rec); err != nil {
		return err
	}
	w.pending[env.EventID] = env
	w.order = append(w.order, env.EventID)
	return w.maybeCompactLocked()
}

// Ack 标记已确认；未知 event_id 视为已确认。
func (w *WAL) Ack(eventID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.pending[eventID]; !ok {
		return nil
	}
	if err := w.appendLocked(record{Op: "ack", EventID: eventID}); err != nil {
		return err
	}
	delete(w.pending, eventID)
	return w.maybeCompactLocked()
}

// Pending 按入队顺序返回未确认事件。
func (w *WAL) Pending() []wire.Envelope {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]wire.Envelope, 0, len(w.pending))
	for _, id := range w.order {
		if env, ok := w.pending[id]; ok {
			out = append(out, env)
		}
	}
	return out
}

// Len 返回未确认事件数。
func (w *WAL) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending)
}

// Size 返回当前文件字节数。
func (w *WAL) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Close 关闭文件。
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *WAL) appendLocked(rec record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := w.file.Write(data)
	if err != nil {
		return fmt.Errorf("queue: failed to write WAL: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("queue: failed to fsync WAL: %w", err)
	}
	w.size += int64(n)
	return nil
}

func (w *WAL) maybeCompactLocked() error {
	if len(w.pending) == 0 && w.size == 0 {
		return nil
	}
	if w.size < w.opts.MaxBytes && len(w.pending) < w.opts.MaxRecords {
		return nil
	}
	return w.compactLocked()
}

// compactLocked 用原子替换把文件重写为仅含 pending 的内容。
func (w *WAL) compactLocked() error {
	var buf bytes.Buffer
	for _, id := range w.order {
		env, ok := w.pending[id]
		if !ok {
			continue
		}
		rec := record{Op: "put", EventID: id, Envelope: &env, EnqueuedAt: wire.NowUnix()}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}

	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	if err := atomicWrite(w.path, buf.Bytes()); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	w.size = int64(buf.Len())

	// 清理 order 中的 ack 残留，避免无限增长。
	kept := w.order[:0]
	for _, id := range w.order {
		if _, ok := w.pending[id]; ok {
			kept = append(kept, id)
		}
	}
	w.order = kept
	return nil
}

// atomicWrite 与 config.WriteAtomic 语义一致，但本包不反向依赖 config。
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
