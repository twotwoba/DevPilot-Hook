// Package event 提供审批状态机与事件节流。
package event

import (
	"context"
	"sync"
	"time"

	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// Status 是审批状态。
type Status string

const (
	StatusPending   Status = "pending"
	StatusApproved  Status = "approved"
	StatusDenied    Status = "denied"
	StatusExpired   Status = "expired"
	StatusCancelled Status = "cancelled"
	StatusUnknown   Status = "unknown"
)

// Approval 是一条等待手机决策的审批。
type Approval struct {
	ID        string
	Envelope  wire.Envelope
	CreatedAt time.Time
	Deadline  time.Time

	// 以下字段由 Registry.mu 保护。
	status   Status
	decision wire.DecisionKind
	done     chan struct{}
}

// Result 是 Wait 的返回值。
type Result struct {
	Status   Status
	Decision wire.DecisionKind
}

// DecideResult 是 Decide 的返回值。
type DecideResult struct {
	Status   Status
	Decision wire.DecisionKind
	// Accepted 表示本次调用真正改变了状态；false 表示重复决策或已过期。
	Accepted bool
}

// Registry 保存本 daemon 所有 pending 审批。
//
// 只存内存：daemon 崩溃后 hook 会因超时回落原生询问，这是安全的方向。
type Registry struct {
	mu    sync.Mutex
	items map[string]*Approval
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]*Approval)}
}

// Add 登记一条审批。同一 approval_id 已存在时返回已有对象与 false。
func (r *Registry) Add(env wire.Envelope, now time.Time, ttl time.Duration) (*Approval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.items[env.ApprovalID]; ok {
		return a, false
	}
	a := &Approval{
		ID:        env.ApprovalID,
		Envelope:  env,
		CreatedAt: now,
		Deadline:  now.Add(ttl),
		status:    StatusPending,
		done:      make(chan struct{}),
	}
	r.items[env.ApprovalID] = a
	return a, true
}

// Wait 阻塞直到决策到达、超时或调用方取消。
//
// 超时只产生 expired；调用方断开产生 cancelled。两者都意味着「无决策」。
func (r *Registry) Wait(ctx context.Context, id string) Result {
	r.mu.Lock()
	a, ok := r.items[id]
	if !ok {
		r.mu.Unlock()
		return Result{Status: StatusUnknown}
	}
	if a.status != StatusPending {
		res := Result{Status: a.status, Decision: a.decision}
		r.mu.Unlock()
		return res
	}
	r.mu.Unlock()

	timer := time.NewTimer(time.Until(a.Deadline))
	defer timer.Stop()

	select {
	case <-a.done:
		r.mu.Lock()
		res := Result{Status: a.status, Decision: a.decision}
		r.mu.Unlock()
		return res
	case <-timer.C:
		r.finish(a, StatusExpired, "")
		return Result{Status: StatusExpired}
	case <-ctx.Done():
		r.finish(a, StatusCancelled, "")
		return Result{Status: StatusCancelled}
	}
}

// Decide 提交手机决策；first-wins。
func (r *Registry) Decide(id string, d wire.DecisionKind, now time.Time) DecideResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.items[id]
	if !ok {
		return DecideResult{Status: StatusUnknown}
	}
	if a.status != StatusPending {
		return DecideResult{Status: a.status, Decision: a.decision}
	}
	if now.After(a.Deadline) {
		r.finishLocked(a, StatusExpired, "")
		return DecideResult{Status: StatusExpired}
	}
	status := StatusApproved
	if d == wire.DecisionDeny {
		status = StatusDenied
	}
	r.finishLocked(a, status, d)
	return DecideResult{Status: status, Decision: d, Accepted: true}
}

// Expire 清理所有已超时的 pending 审批，返回本次过期的列表。
func (r *Registry) Expire(now time.Time) []*Approval {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Approval
	for _, a := range r.items {
		if a.status == StatusPending && now.After(a.Deadline) {
			r.finishLocked(a, StatusExpired, "")
			out = append(out, a)
		}
	}
	return out
}

// Cancel 取消一条审批（例如 hook 进程退出）。
func (r *Registry) Cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.items[id]; ok && a.status == StatusPending {
		r.finishLocked(a, StatusCancelled, "")
	}
}

// Has 判断审批是否存在。
func (r *Registry) Has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.items[id]
	return ok
}

// Len 返回当前保留的审批数量（含已结束但尚未清理的）。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

// Pending 返回所有仍处于 pending 的审批快照。
func (r *Registry) Pending() []wire.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]wire.Envelope, 0, len(r.items))
	for _, a := range r.items {
		if a.status == StatusPending {
			out = append(out, a.Envelope)
		}
	}
	return out
}

// Cleanup 删除已结束且超过 keep 的审批，返回删除数量。
func (r *Registry) Cleanup(now time.Time, keep time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for id, a := range r.items {
		if a.status != StatusPending && now.Sub(a.Deadline) > keep {
			delete(r.items, id)
			removed++
		}
	}
	return removed
}

func (r *Registry) finish(a *Approval, status Status, d wire.DecisionKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishLocked(a, status, d)
}

// finishLocked 要求调用方已持有 r.mu。
func (r *Registry) finishLocked(a *Approval, status Status, d wire.DecisionKind) {
	if a.status != StatusPending {
		return
	}
	a.status = status
	a.decision = d
	close(a.done)
}
