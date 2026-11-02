package control

import (
	"context"
	"sync"
)

// Queue hands items to one consumer. Push never blocks: the controller calls it
// under its lock, so a bounded queue reports a full push instead of waiting.
type Queue[T any] struct {
	mu    sync.Mutex
	items []T
	limit int // 0 means unbounded
	wake  chan struct{}
}

// NewQueue returns a queue holding at most limit items; limit 0 is unbounded.
func NewQueue[T any](limit int) *Queue[T] {
	return &Queue[T]{limit: limit, wake: make(chan struct{}, 1)}
}

// Push appends item and reports false when a bounded queue is full.
func (q *Queue[T]) Push(item T) bool {
	q.mu.Lock()
	if q.limit > 0 && len(q.items) >= q.limit {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// TryPop returns the oldest item without blocking.
func (q *Queue[T]) TryPop() (item T, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return item, false
	}
	item, q.items = q.items[0], q.items[1:]
	return item, true
}

// Pop blocks until an item arrives; false means ctx ended first.
func (q *Queue[T]) Pop(ctx context.Context) (T, bool) {
	for {
		if item, ok := q.TryPop(); ok {
			return item, true
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, false
		case <-q.wake:
		}
	}
}
