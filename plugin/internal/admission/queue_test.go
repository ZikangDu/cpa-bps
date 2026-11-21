package admission

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestFIFOWithCancellation(t *testing.T) {
	q := New()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const account = "fixture"
	first := mustAcquire(t, q, ctx, account, 1)
	defer first()

	second := acquireAsync(q, ctx, account)
	waitForQueued(t, ctx, q, account, 1)
	canceledCtx, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	canceled := acquireAsync(q, canceledCtx, account)
	waitForQueued(t, ctx, q, account, 2)
	last := acquireAsync(q, ctx, account)
	waitForQueued(t, ctx, q, account, 3)
	cancelWaiting()
	if result := await(t, ctx, canceled); !errors.Is(result.err, context.Canceled) || result.release != nil {
		t.Fatalf("canceled waiter got release=%t, error=%v", result.release != nil, result.err)
	}

	first()
	secondResult := await(t, ctx, second)
	if secondResult.err != nil {
		t.Fatal(secondResult.err)
	}
	defer secondResult.release()
	first()
	if count := queued(q, account); count != 1 {
		t.Fatalf("double release admitted the last waiter: queued=%d", count)
	}
	secondResult.release()
	lastResult := await(t, ctx, last)
	if lastResult.err != nil {
		t.Fatal(lastResult.err)
	}
	lastResult.release()
	assertDrained(t, q)
}

func TestCancellationRacesWithGrant(t *testing.T) {
	q := New()
	ctx, cancelTest := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelTest()
	const account = "fixture"
	for i := 0; i < 100; i++ {
		first := mustAcquire(t, q, ctx, account, 1)
		waitingCtx, cancelWaiting := context.WithCancel(ctx)
		waiting := acquireAsync(q, waitingCtx, account)
		waitForQueued(t, ctx, q, account, 1)
		released := make(chan struct{})
		go func() {
			first()
			close(released)
		}()
		cancelWaiting()
		result := await(t, ctx, waiting)
		switch {
		case result.err == nil:
			result.release()
		case !errors.Is(result.err, context.Canceled) || result.release != nil:
			t.Fatalf("request %d: release=%t, error=%v", i, result.release != nil, result.err)
		}
		select {
		case <-released:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertDrained(t, q)
	}
}

func TestLimitsArePerAccountAndCanChangeAfterDrain(t *testing.T) {
	var q Queue
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a := mustAcquire(t, &q, ctx, "alpha", 1)
	x := mustAcquire(t, &q, ctx, "beta", 0)
	y := mustAcquire(t, &q, ctx, "beta", 0)
	if release, err := q.Acquire(ctx, "alpha", 0); !errors.Is(err, ErrLimitChanged) || release != nil {
		t.Fatalf("limit change got release=%t, error=%v", release != nil, err)
	}
	deadline, cancelDeadline := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelDeadline()
	if release, err := q.Acquire(deadline, "alpha", 1); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatalf("expired context got release=%t, error=%v", release != nil, err)
	}
	a()
	mustAcquire(t, &q, ctx, "alpha", 2)()
	y()
	x()
	assertDrained(t, &q)
}

func TestSetLimitAppliesToActiveQueueAndStaleCallers(t *testing.T) {
	q := New()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := mustAcquire(t, q, ctx, "fixture", 1)
	defer first()
	second := acquireAsync(q, ctx, "fixture")
	waitForQueued(t, ctx, q, "fixture", 1)
	if got := q.Snapshot("fixture", 20); got.Active != 1 || got.Waiting != 1 || got.Limit != 1 {
		t.Fatalf("unexpected pending snapshot: %+v", got)
	}
	if err := q.SetLimit("fixture", 2); err != nil {
		t.Fatal(err)
	}
	next := await(t, ctx, second)
	if next.err != nil {
		t.Fatal(next.err)
	}
	defer next.release()
	if got := q.Snapshot("fixture", 20); got.Active != 2 || got.Waiting != 0 || got.Limit != 2 {
		t.Fatalf("raising limit failed: %+v", got)
	}
	if err := q.SetLimit("fixture", 1); err != nil {
		t.Fatal(err)
	}
	third := make(chan acquisition, 1)
	go func() {
		// This caller read the old default before the administrator's update.
		release, err := q.Acquire(ctx, "fixture", 20)
		third <- acquisition{release, err}
	}()
	waitForQueued(t, ctx, q, "fixture", 1)
	first()
	if got := q.Snapshot("fixture", 20); got.Active != 1 || got.Waiting != 1 || got.Limit != 1 {
		t.Fatalf("lowered limit was lost: %+v", got)
	}
	next.release()
	last := await(t, ctx, third)
	if last.err != nil {
		t.Fatal(last.err)
	}
	last.release()
	if got := q.Snapshot("fixture", 20); got.Active != 0 || got.Waiting != 0 || got.Limit != 1 {
		t.Fatalf("saved limit did not survive drain: %+v", got)
	}
}

func TestSetUnlimitedAdmitsQueuedWork(t *testing.T) {
	q := New()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := mustAcquire(t, q, ctx, "fixture", 1)
	defer first()
	waiting := acquireAsync(q, ctx, "fixture")
	waitForQueued(t, ctx, q, "fixture", 1)
	if err := q.SetLimit("fixture", 0); err != nil {
		t.Fatal(err)
	}
	next := await(t, ctx, waiting)
	if next.err != nil {
		t.Fatal(next.err)
	}
	next.release()
	if got := q.Snapshot("fixture", 1); got.Active != 1 || got.Limit != 0 {
		t.Fatalf("unlimited override was not retained: %+v", got)
	}
}

type acquisition struct {
	release func()
	err     error
}

func acquireAsync(q *Queue, ctx context.Context, account string) <-chan acquisition {
	result := make(chan acquisition, 1)
	go func() {
		release, err := q.Acquire(ctx, account, 1)
		result <- acquisition{release, err}
	}()
	return result
}

func await(t *testing.T, ctx context.Context, ch <-chan acquisition) acquisition {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return acquisition{}
	}
}

func mustAcquire(t *testing.T, q *Queue, ctx context.Context, account string, limit int) func() {
	t.Helper()
	release, err := q.Acquire(ctx, account, limit)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func waitForQueued(t *testing.T, ctx context.Context, q *Queue, account string, want int) {
	t.Helper()
	for queued(q, account) != want {
		if err := ctx.Err(); err != nil {
			t.Fatalf("waiting for %d queued requests: %v", want, err)
		}
		runtime.Gosched()
	}
}

func queued(q *Queue, account string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if state := q.accounts[account]; state != nil {
		return state.waiting.Len()
	}
	return 0
}

func assertDrained(t *testing.T, q *Queue) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.accounts) != 0 {
		t.Fatalf("retained %d account states after drain", len(q.accounts))
	}
}
