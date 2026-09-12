package session

import (
	"github.com/kisara71/luma/internal/utils"
	"sync"
	"time"
)

type pendingThink struct {
	timer      *time.Timer
	proactive  bool
	generation uint64
}

type Session[T any] struct {
	Ref    Ref
	Buffer *utils.RingBuffer[T]

	mu              sync.RWMutex
	pending         *pendingThink
	processing      bool
	nextProactiveAt time.Time
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

func (s *Session[T]) SchedulePending(debounce time.Duration, proactive bool, onFlush func(generation uint64)) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending != nil {
		// A real incoming message takes precedence over a scheduled reflection
		// when both land in the debounce window.
		s.pending.proactive = s.pending.proactive && proactive
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
		proactive:  proactive,
		generation: 1,
	}
	gen := pending.generation
	pending.timer = time.AfterFunc(debounce, func() {
		onFlush(gen)
	})
	s.pending = pending
}

func (s *Session[T]) ConsumePending(generation uint64) (proactive bool, ok bool) {
	if s == nil {
		return false, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pending == nil || s.pending.generation != generation {
		return false, false
	}

	proactive = s.pending.proactive
	s.pending = nil
	return proactive, true
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

func (s *Session[T]) TryBeginProcessing() bool {
	if s == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.processing {
		return false
	}

	s.processing = true
	return true
}

func (s *Session[T]) FinishProcessing() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.processing = false
}

func (s *Session[T]) ScheduleProactive(at time.Time) {
	if s == nil || at.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.nextProactiveAt.IsZero() {
		return
	}
	s.nextProactiveAt = at
}

func (s *Session[T]) ProactiveDue(now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.processing && !s.nextProactiveAt.IsZero() && !now.Before(s.nextProactiveAt)
}

func (s *Session[T]) ClearProactivePlan() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextProactiveAt = time.Time{}
}
