package adapter

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"cpa-bps-reconstruction/internal/documents"
)

type documentWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *documentWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestDocumentCanceledOwnerHandsOffWithoutPoisoningWaiters(t *testing.T) {
	p := newDocumentProcesses()
	job := documents.Job{Filename: "sales.csv", Data: []byte("a,b\n1,2")}
	owner, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	started, replaced, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	run := func(ctx context.Context, _, _ string, _ documents.Job) (documents.Result, error) {
		switch calls.Add(1) {
		case 1:
			close(started)
			<-ctx.Done()
			// A completed result racing with cancellation is not reusable.
			return documents.Result{Text: "canceled owner"}, nil
		case 2:
			close(replaced)
			<-release
			return documents.Result{Text: "valid replacement"}, nil
		default:
			t.Error("multiple replacement parsers started")
			return documents.Result{}, errors.New("unexpected parser")
		}
	}
	ownerResult := make(chan error, 1)
	go func() {
		_, err := p.parseWithWorker(owner, "worker", "python", "caller", job, run)
		ownerResult <- err
	}()
	<-started
	var waiters sync.WaitGroup
	for range 20 {
		ctx := &documentWaitContext{Context: context.Background(), waiting: make(chan struct{})}
		waiters.Go(func() {
			result, err := p.parseWithWorker(ctx, "worker", "python", "caller", job, run)
			if err != nil || result.Text != "valid replacement" {
				t.Errorf("live waiter received another request's cancellation: %q, %v", result.Text, err)
			}
		})
		<-ctx.waiting
	}
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner error = %v", err)
	}
	<-replaced
	close(release)
	waiters.Wait()
	if calls.Load() != 2 || len(p.entries) != 1 || len(p.slots) != 0 || p.size != len("valid replacement") {
		t.Fatalf("unexpected cache state: calls=%d entries=%d slots=%d bytes=%d", calls.Load(), len(p.entries), len(p.slots), p.size)
	}
}

func TestDocumentCanceledWaiterDoesNotCancelOwnerOrReadCache(t *testing.T) {
	p := newDocumentProcesses()
	job := documents.Job{Filename: "sales.csv", Data: []byte("a,b\n1,2")}
	started, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	run := func(context.Context, string, string, documents.Job) (documents.Result, error) {
		calls.Add(1)
		close(started)
		<-release
		return documents.Result{Text: "cached"}, nil
	}
	go func() {
		defer close(completed)
		if _, err := p.parseWithWorker(context.Background(), "worker", "", "caller", job, run); err != nil {
			t.Error(err)
		}
	}()
	<-started
	base, cancel := context.WithCancel(context.Background())
	ctx := &documentWaitContext{Context: base, waiting: make(chan struct{})}
	waited := make(chan error, 1)
	go func() {
		_, err := p.parseWithWorker(ctx, "worker", "", "caller", job, run)
		waited <- err
	}()
	<-ctx.waiting
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	<-completed
	if _, err := p.parseWithWorker(ctx, "worker", "", "caller", job, run); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller read a cached success: %v", err)
	}
	result, err := p.parseWithWorker(context.Background(), "worker", "", "caller", job, run)
	if err != nil || result.Text != "cached" || calls.Load() != 1 {
		t.Fatal("waiter cancellation disrupted the owner or its cache", err)
	}
}

func TestDocumentCanceledQueuedOwnerDoesNotPoisonNextAttempt(t *testing.T) {
	p := newDocumentProcesses()
	// Fill worker slots without using wall-clock synchronization.
	p.slots <- struct{}{}
	p.slots <- struct{}{}
	base, cancel := context.WithCancel(context.Background())
	ctx := &documentWaitContext{Context: base, waiting: make(chan struct{})}
	job := documents.Job{Filename: "sales.csv", Data: []byte("a,b\n1,2")}
	var calls atomic.Int32
	run := func(context.Context, string, string, documents.Job) (documents.Result, error) {
		calls.Add(1)
		return documents.Result{Text: "retry succeeded"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.parseWithWorker(ctx, "worker", "", "caller", job, run)
		done <- err
	}()
	<-ctx.waiting
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(p.entries) != 0 {
		t.Fatal("canceled queued document started or cached a parser")
	}
	<-p.slots
	<-p.slots
	result, err := p.parseWithWorker(context.Background(), "worker", "", "caller", job, run)
	if err != nil || result.Text != "retry succeeded" || calls.Load() != 1 {
		t.Fatal("next attempt inherited a canceled owner's failure", err)
	}
}

func TestDocumentWorkerPreservesCallerCancellation(t *testing.T) {
	_, cfg, _ := documentTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runDocumentWorker(ctx, cfg.DocumentWorkerPath, "", documents.Job{Filename: "x.csv", Data: []byte("a,b")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation became %v", err)
	}
}
