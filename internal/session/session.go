package session

import (
	"mumu-bot/internal/utils"
	"sync"
	"time"
)

type pendingThink struct {
	timer      *time.Timer
	isMention  bool
	fromLoop   bool
	generation uint64
}

type Session[T any] struct {
	Ref    Ref
	Buffer *utils.RingBuffer[T]

	mu                sync.RWMutex
	pending           *pendingThink
	processing        bool
	lastProcessedTime time.Time
	lastLoopThinkTime time.Time
}

func NewSession[T any](ref Ref, capacity int) *Session[T] {
	return &Session[T]{
		Ref:    ref,
		Buffer: utils.NewRingBuffer[T](capacity),
	}
}

func (s *Session[T]) Push(item T) {
	if s == nil || s.Buffer == nil {
		return
	}
	s.Buffer.Push(item)
}

func (s *Session[T]) Messages() []T {
	if s == nil || s.Buffer == nil || s.Buffer.IsEmpty() {
		return nil
	}
	return s.Buffer.GetAll()
}

func (s *Session[T]) IsEmpty() bool {
	return s == nil || s.Buffer == nil || s.Buffer.IsEmpty()
}

func (s *Session[T]) SchedulePending(debounce time.Duration, isMention bool, fromLoop bool, onFlush func(generation uint64)) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending != nil {
		s.pending.isMention = s.pending.isMention || isMention
		s.pending.fromLoop = s.pending.fromLoop || fromLoop
		s.pending.generation++
		if s.pending.timer != nil {
			s.pending.timer.Stop()
		}
		gen := s.pending.generation
		s.pending.timer = time.AfterFunc(debounce, func() {
			onFlush(gen)
		})
		return
	}

	pending := &pendingThink{
		isMention:  isMention,
		fromLoop:   fromLoop,
		generation: 1,
	}
	gen := pending.generation
	pending.timer = time.AfterFunc(debounce, func() {
		onFlush(gen)
	})
	s.pending = pending
}

func (s *Session[T]) ConsumePending(generation uint64) (isMention bool, fromLoop bool, ok bool) {
	if s == nil {
		return false, false, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending == nil || s.pending.generation != generation {
		return false, false, false
	}

	isMention = s.pending.isMention
	fromLoop = s.pending.fromLoop
	s.pending = nil
	return isMention, fromLoop, true
}

func (s *Session[T]) ClearPending() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending != nil && s.pending.timer != nil {
		s.pending.timer.Stop()
	}
	s.pending = nil
}

func (s *Session[T]) TryBeginProcessing(fromLoop bool) (lastProcessedTime time.Time, ok bool) {
	if s == nil {
		return time.Time{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.processing {
		return time.Time{}, false
	}

	s.processing = true
	lastProcessedTime = s.lastProcessedTime
	if fromLoop {
		s.lastLoopThinkTime = time.Now()
	} else {
		s.lastProcessedTime = time.Now()
	}
	return lastProcessedTime, true
}

func (s *Session[T]) FinishProcessing() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.processing = false
}

func (s *Session[T]) LastLoopThinkTime() time.Time {
	if s == nil {
		return time.Time{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastLoopThinkTime
}
