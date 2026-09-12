package agent

import (
	"context"
	"github.com/kisara71/luma/internal/session"
	"sync"

	"go.uber.org/zap"
)

// ConcurrencyManager 并发管理器
type ConcurrencyManager struct {
	ctx            context.Context
	cancel         context.CancelFunc
	maxConcurrency int
	currentRunning int
	queue          []*ThinkTask
	active         map[string]bool
	queued         map[string]*ThinkTask
	closed         bool
	mu             sync.Mutex
	wg             sync.WaitGroup

	handler func(ref session.Ref, proactive bool) // 执行函数
}

// ThinkTask 思考任务
type ThinkTask struct {
	Ref       session.Ref
	Proactive bool
}

// NewConcurrencyManager 创建并发管理器
func NewConcurrencyManager(parent context.Context, max int, h func(ref session.Ref, proactive bool)) *ConcurrencyManager {
	ctx, cancel := context.WithCancel(parent)
	return &ConcurrencyManager{
		ctx:            ctx,
		cancel:         cancel,
		maxConcurrency: max,
		currentRunning: 0,
		active:         make(map[string]bool),
		queued:         make(map[string]*ThinkTask),
		handler:        h,
	}
}

// Submit 提交任务
func (m *ConcurrencyManager) Submit(ref session.Ref, proactive bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return
	}

	if pending := m.queued[ref.ID()]; pending != nil {
		// A real incoming message always wins over a proactive task.
		pending.Proactive = pending.Proactive && proactive
		return
	}

	// Keep one follow-up for a conversation that receives new input while its
	// previous generation is still running.
	if m.active[ref.ID()] || (m.maxConcurrency > 0 && m.currentRunning >= m.maxConcurrency) {
		task := &ThinkTask{
			Ref:       ref,
			Proactive: proactive,
		}
		m.queue = append(m.queue, task)
		m.queued[ref.ID()] = task
		zap.L().Debug("并发已满，任务进入队列",
			zap.String("id", ref.ID()),
			zap.Int("current", m.currentRunning),
			zap.Int("queue_len", len(m.queue)))
		return
	}

	m.currentRunning++
	m.active[ref.ID()] = true
	m.wg.Add(1)
	go m.execute(ref, proactive)
}

// execute 执行任务
func (m *ConcurrencyManager) execute(ref session.Ref, proactive bool) {
	defer m.wg.Done()
	defer m.Finish(ref)
	if err := m.ctx.Err(); err != nil {
		return
	}
	m.handler(ref, proactive)
}

// Finish 任务完成回调
func (m *ConcurrencyManager) Finish(ref session.Ref) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.currentRunning--
	delete(m.active, ref.ID())
	if m.currentRunning < 0 {
		m.currentRunning = 0
	}

	// 调度下一个任务
	if len(m.queue) > 0 && !m.closed && m.ctx.Err() == nil {
		// 取出队首任务
		task := m.queue[0]
		m.queue = m.queue[1:]
		delete(m.queued, task.Ref.ID())

		// 立即启动
		m.currentRunning++
		m.active[task.Ref.ID()] = true
		m.wg.Add(1)
		go m.execute(task.Ref, task.Proactive)
		zap.L().Debug("从队列调度任务执行", zap.Int64("user_id", task.Ref.UserID))
	}
}

// Close 停止调度并等待已启动任务退出。
func (m *ConcurrencyManager) Close() {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	m.closed = true
	m.queue = nil
	m.active = make(map[string]bool)
	m.queued = make(map[string]*ThinkTask)
	m.mu.Unlock()

	m.wg.Wait()
}
