package agent

import (
	"context"
	"mumu-bot/internal/memory"
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
	inQueue        map[string]bool // 快速去重（会话key -> 是否在队列中）
	mu             sync.Mutex
	wg             sync.WaitGroup

	handler func(ref memory.ConversationRef, isMention bool) // 执行函数
}

// ThinkTask 思考任务
type ThinkTask struct {
	Ref       memory.ConversationRef
	IsMention bool
}

// NewConcurrencyManager 创建并发管理器
func NewConcurrencyManager(parent context.Context, max int, h func(ref memory.ConversationRef, isMention bool)) *ConcurrencyManager {
	ctx, cancel := context.WithCancel(parent)
	return &ConcurrencyManager{
		ctx:            ctx,
		cancel:         cancel,
		maxConcurrency: max,
		currentRunning: 0,
		inQueue:        make(map[string]bool),
		handler:        h,
	}
}

// Submit 提交任务
func (m *ConcurrencyManager) Submit(ref memory.ConversationRef, isMention bool) {
	if err := m.ctx.Err(); err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.inQueue[ref.ID()] {
		zap.L().Debug("任务已在队列中，跳过", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()))
		return
	}

	// 如果设置了最大并发数，且当前运行数已满，则入队
	if m.maxConcurrency > 0 && m.currentRunning >= m.maxConcurrency {
		m.queue = append(m.queue, &ThinkTask{
			Ref:       ref,
			IsMention: isMention,
		})
		m.inQueue[ref.ID()] = true
		zap.L().Debug("并发已满，任务进入队列",
			zap.String("source", string(ref.Source)),
			zap.String("id", ref.ID()),
			zap.Int("current", m.currentRunning),
			zap.Int("queue_len", len(m.queue)))
		return
	}

	m.currentRunning++
	m.wg.Add(1)
	go m.execute(ref, isMention)
}

// execute 执行任务
func (m *ConcurrencyManager) execute(ref memory.ConversationRef, isMention bool) {
	defer m.wg.Done()
	defer m.Finish()
	if err := m.ctx.Err(); err != nil {
		return
	}
	m.handler(ref, isMention)
}

// Finish 任务完成回调
func (m *ConcurrencyManager) Finish() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.currentRunning--
	if m.currentRunning < 0 {
		m.currentRunning = 0
	}

	// 调度下一个任务
	if len(m.queue) > 0 && m.ctx.Err() == nil {
		// 取出队首任务
		task := m.queue[0]
		m.queue = m.queue[1:]
		delete(m.inQueue, task.Ref.ID())

		// 立即启动
		m.currentRunning++
		m.wg.Add(1)
		go m.execute(task.Ref, task.IsMention)
		zap.L().Debug("从队列调度任务执行", zap.String("source", string(task.Ref.Source)), zap.String("id", task.Ref.ID()))
	}
}

// Close 停止调度并等待已启动任务退出。
func (m *ConcurrencyManager) Close() {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	m.queue = nil
	m.inQueue = make(map[string]bool)
	m.mu.Unlock()

	m.wg.Wait()
}
