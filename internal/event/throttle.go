package event

import (
	"sync"
	"time"
)

// Throttle 是按键（会话 + 事件类型）的固定窗口节流器。
//
// 只用于 tool_running / tool_finished 这类高频遥测；审批与任务完成不经过这里。
type Throttle struct {
	mu       sync.Mutex
	interval time.Duration
	last     map[string]time.Time
}

// NewThrottle 创建节流器；interval <= 0 表示全部放行。
func NewThrottle(interval time.Duration) *Throttle {
	return &Throttle{interval: interval, last: make(map[string]time.Time)}
}

// Allow 判断 key 在 now 时刻是否允许放行。
func (t *Throttle) Allow(key string, now time.Time) bool {
	if t.interval <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[key]; ok && now.Sub(last) < t.interval {
		return false
	}
	t.last[key] = now
	return true
}

// Forget 清掉某会话的节流状态（SessionEnd 时调用，避免 map 无界增长）。
func (t *Throttle) Forget(prefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.last {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(t.last, k)
		}
	}
}

// Len 返回当前记录的 key 数，供测试与诊断。
func (t *Throttle) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.last)
}
