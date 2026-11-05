package tui

import (
	"context"
	"sync"

	"dashy/internal/control"
)

// eventQueue hands controller events to one consumer. Push never blocks, because the controller
// calls it under its lock.
type eventQueue struct {
	mu    sync.Mutex
	items []control.Event
	wake  chan struct{}
}

func newEventQueue() *eventQueue {
	return &eventQueue{wake: make(chan struct{}, 1)}
}

func (q *eventQueue) Push(item control.Event) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) TryPop() (item control.Event, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return item, false
	}
	item, q.items = q.items[0], q.items[1:]
	return item, true
}

func (q *eventQueue) Pop(ctx context.Context) (control.Event, bool) {
	for {
		if item, ok := q.TryPop(); ok {
			return item, true
		}
		select {
		case <-ctx.Done():
			var zero control.Event
			return zero, false
		case <-q.wake:
		}
	}
}
