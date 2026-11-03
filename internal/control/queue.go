package control

import (
	"context"
	"sync"
)

// Queue hands items to one consumer. Push never blocks, because the controller
// calls it under its lock.
type Queue[T any] struct {
	mu    sync.Mutex
	items []T
	wake  chan struct{}
}

// NewQueue returns an empty, unbounded queue.
func NewQueue[T any]() *Queue[T] {
	return &Queue[T]{wake: make(chan struct{}, 1)}
}

// Push appends item.
func (q *Queue[T]) Push(item T) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
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
