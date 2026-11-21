package admission

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// ErrLimitChanged is returned when an account already has active or waiting
// requests under a different limit. Its limit can change once that work drains.
var ErrLimitChanged = errors.New("account concurrency limit changed while requests are pending")

// Queue admits work independently for each account. Its zero value is ready to use.
type Queue struct {
	mu       sync.Mutex
	accounts map[string]*accountQueue
	limits   map[string]int
}

// Snapshot reports the current admission state for one account.
type Snapshot struct {
	Active  int `json:"active"`
	Waiting int `json:"waiting"`
	Limit   int `json:"limit"`
}

type accountQueue struct {
	limit   int
	active  int
	waiting list.List
}

type waiter struct {
	ctx   context.Context
	ready chan struct{}
}

func New() *Queue {
	return &Queue{}
}

// Acquire waits for a slot in the account's FIFO queue. Zero allows unlimited
// requests. The caller must release an acquired slot when it finishes.
func (q *Queue) Acquire(ctx context.Context, account string, limit int) (func(), error) {
	if limit < 0 {
		return nil, errors.New("account concurrency limit cannot be negative")
	}

	q.mu.Lock()
	if err := ctx.Err(); err != nil {
		q.mu.Unlock()
		return nil, err
	}
	if override, exists := q.limits[account]; exists {
		limit = override
	}
	state := q.accounts[account]
	if state == nil {
		if q.accounts == nil {
			q.accounts = make(map[string]*accountQueue)
		}
		state = &accountQueue{limit: limit}
		q.accounts[account] = state
	} else if state.limit != limit {
		q.mu.Unlock()
		return nil, ErrLimitChanged
	}

	release := sync.OnceFunc(func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		state.active--
		q.advance(account, state)
	})
	if state.waiting.Len() == 0 && (limit == 0 || state.active < limit) {
		state.active++
		q.mu.Unlock()
		return release, nil
	}

	next := waiter{ctx: ctx, ready: make(chan struct{})}
	element := state.waiting.PushBack(next)
	q.mu.Unlock()
	select {
	case <-next.ready:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		q.mu.Lock()
		select {
		case <-next.ready:
			state.active--
		default:
			state.waiting.Remove(element)
		}
		q.advance(account, state)
		q.mu.Unlock()
		return nil, err
	}
	return release, nil
}

// SetLimit changes one account's capacity while retaining admitted requests.
// Raising the limit immediately admits waiting work in its existing FIFO order.
// Future Acquire calls use this override, including callers that read an older
// configured limit before the update.
func (q *Queue) SetLimit(account string, limit int) error {
	if account == "" || limit < 0 {
		return errors.New("account concurrency limit is invalid")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limits == nil {
		q.limits = make(map[string]int)
	}
	q.limits[account] = limit
	if state := q.accounts[account]; state != nil {
		state.limit = limit
		q.advance(account, state)
	}
	return nil
}

func (q *Queue) Snapshot(account string, defaultLimit int) Snapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	result := Snapshot{Limit: defaultLimit}
	if limit, exists := q.limits[account]; exists {
		result.Limit = limit
	}
	if state := q.accounts[account]; state != nil {
		result.Limit = state.limit
		result.Active = state.active
		for entry := state.waiting.Front(); entry != nil; entry = entry.Next() {
			if entry.Value.(waiter).ctx.Err() == nil {
				result.Waiting++
			}
		}
	}
	return result
}

// advance runs with q.mu held. A canceled waiter's goroutine may clean up
// after the account has drained and another caller has already reused its ID.
func (q *Queue) advance(account string, state *accountQueue) {
	for state.waiting.Len() > 0 && (state.limit == 0 || state.active < state.limit) {
		next := state.waiting.Remove(state.waiting.Front()).(waiter)
		if next.ctx.Err() != nil {
			continue
		}
		state.active++
		close(next.ready)
	}
	if state.active == 0 && state.waiting.Len() == 0 && q.accounts[account] == state {
		delete(q.accounts, account)
	}
}
