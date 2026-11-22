package upstream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAttachmentCacheDeduplicatesConcurrentUploads(t *testing.T) {
	cache := newAttachmentCache()
	key := sha256.Sum256([]byte("fixture"))
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	upload := func() (string, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return "file-fixture", nil
	}
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			id, err := cache.get(context.Background(), key, upload)
			if err != nil || id != "file-fixture" {
				t.Errorf("duplicate uploader returned %q, %v", id, err)
			}
		})
	}
	<-started
	close(release)
	group.Wait()
	if calls.Load() != 1 || len(cache.entries) != 1 || len(cache.pending) != 0 {
		t.Fatal("identical concurrent images did not share one completed upload")
	}
}

func TestAttachmentCacheKeepsCredentialOriginAndContentSeparate(t *testing.T) {
	image := inlineImage{content: &inlineImageContent{mimeType: "image/png", digest: sha256.Sum256([]byte("fixture-image"))}}
	base := attachmentCacheKey("https://origin.example/files", "account-a-token", image, false)
	otherContent := image
	otherContent.content = &inlineImageContent{mimeType: "image/png", digest: sha256.Sum256([]byte("different-image"))}
	otherFormat := image
	otherFormat.content = &inlineImageContent{mimeType: "image/jpeg", digest: image.content.digest}
	keys := [][32]byte{
		attachmentCacheKey("https://other.example/files", "account-a-token", image, false),
		attachmentCacheKey("https://origin.example/other", "account-a-token", image, false),
		attachmentCacheKey("https://origin.example/files", "account-b-token", image, false),
		attachmentCacheKey("https://origin.example/files", "account-a-rotated-token", image, false),
		attachmentCacheKey("https://origin.example/files", "account-a-token", otherContent, false),
		attachmentCacheKey("https://origin.example/files", "account-a-token", otherFormat, false),
		attachmentCacheKey("https://origin.example/files", "account-a-token", image, true),
	}
	seen := map[[32]byte]bool{base: true}
	for _, key := range keys {
		if seen[key] {
			t.Fatal("attachment cache key crossed credential, origin, content or protocol")
		}
		seen[key] = true
	}
}

func TestAttachmentCacheExpiryIsFixedAndFailuresAreRetriedByNextCall(t *testing.T) {
	cache := newAttachmentCache()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return now }
	key := sha256.Sum256([]byte("fixture"))
	calls := 0
	upload := func() (string, error) {
		calls++
		return fmt.Sprintf("file-fixture-%d", calls), nil
	}
	first, err := cache.get(context.Background(), key, upload)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(attachmentCacheTTL - time.Second)
	stillFirst, err := cache.get(context.Background(), key, upload)
	if err != nil || stillFirst != first || calls != 1 {
		t.Fatal("fresh reference was uploaded again")
	}
	now = now.Add(time.Second)
	second, err := cache.get(context.Background(), key, upload)
	if err != nil || second == first || calls != 2 {
		t.Fatal("cache hit extended the reference lifetime")
	}

	failureKey := sha256.Sum256([]byte("failure"))
	_, err = cache.get(context.Background(), failureKey, func() (string, error) { return "", ErrImageUpload })
	if !errors.Is(err, ErrImageUpload) || cache.entries[failureKey] != nil || cache.pending[failureKey] != nil {
		t.Fatal("failed upload was retained")
	}
	if _, err := cache.get(context.Background(), failureKey, upload); err != nil || calls != 3 {
		t.Fatal("next caller could not attempt an upload after failure")
	}
}

func TestAttachmentCacheWaiterCancellationDoesNotDuplicateOrCancelOwner(t *testing.T) {
	cache := newAttachmentCache()
	key := sha256.Sum256([]byte("fixture"))
	started, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(completed)
		_, err := cache.get(context.Background(), key, func() (string, error) {
			close(started)
			<-release
			return "file-fixture", nil
		})
		if err != nil {
			t.Error(err)
		}
	}()
	<-started
	base, cancel := context.WithCancel(context.Background())
	ctx := &observedWaitContext{Context: base, waiting: make(chan struct{})}
	waited := make(chan error, 1)
	go func() {
		_, err := cache.get(ctx, key, func() (string, error) {
			t.Error("canceled waiter started another upload")
			return "", ErrImageUpload
		})
		waited <- err
	}()
	// Done is accessed only after this caller finds the pending upload.
	<-ctx.waiting
	cancel()
	err := <-waited
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter returned %v", err)
	}
	close(release)
	<-completed
	id, err := cache.get(context.Background(), key, func() (string, error) {
		t.Error("waiter cancellation removed the completed owner upload")
		return "", ErrImageUpload
	})
	if err != nil || id != "file-fixture" {
		t.Fatal("owner upload did not remain usable")
	}
}

func TestAttachmentCacheCanceledOwnerHandsOffToOneLiveWaiter(t *testing.T) {
	cache := newAttachmentCache()
	key := sha256.Sum256([]byte("fixture"))
	ownerContext, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	started, replaced, releaseReplacement := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ownerResult := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, err := cache.get(ownerContext, key, func() (string, error) {
			calls.Add(1)
			close(started)
			<-ownerContext.Done()
			// Even a success racing with cancellation must not publish this ID.
			return "file-canceled-owner", nil
		})
		ownerResult <- err
	}()
	<-started
	var waiters sync.WaitGroup
	for range 20 {
		ctx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
		waiters.Go(func() {
			id, err := cache.get(ctx, key, func() (string, error) {
				if calls.Add(1) == 2 {
					close(replaced)
				} else {
					t.Error("canceled owner caused duplicate replacement uploads")
				}
				<-releaseReplacement
				return "file-live-replacement", nil
			})
			if err != nil || id != "file-live-replacement" {
				t.Errorf("live waiter returned %q, %v", id, err)
			}
		})
		<-ctx.waiting
	}
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Errorf("canceled owner returned %v", err)
	}
	<-replaced
	close(releaseReplacement)
	waiters.Wait()
	if calls.Load() != 2 || len(cache.entries) != 1 || len(cache.pending) != 0 {
		t.Fatal("owner handoff did not coalesce into one successful upload")
	}
	if cache.entries[key].Value.(cachedImage).fileID != "file-live-replacement" {
		t.Fatal("the canceled owner's reference entered the cache")
	}
}

func TestAttachmentCacheWaiterRetriesOnlyOwnerCancellation(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		err     error
		replace bool
	}{
		{"canceled", fmt.Errorf("upload: %w", context.Canceled), true},
		{"deadline", fmt.Errorf("upload: %w", context.DeadlineExceeded), true},
		{"ordinary_failure", ErrImageUpload, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cache := newAttachmentCache()
			key := sha256.Sum256([]byte("fixture"))
			pending := &pendingImage{done: make(chan struct{}), err: fixture.err}
			cache.pending[key] = pending
			ctx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
			var calls atomic.Int32
			result := make(chan error, 1)
			go func() {
				_, err := cache.get(ctx, key, func() (string, error) {
					calls.Add(1)
					return "file-replacement", nil
				})
				result <- err
			}()
			<-ctx.waiting
			cache.mu.Lock()
			delete(cache.pending, key)
			close(pending.done)
			cache.mu.Unlock()
			err := <-result
			if fixture.replace {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("canceled owner was not replaced once: %v, %d uploads", err, calls.Load())
				}
			} else if !errors.Is(err, fixture.err) || calls.Load() != 0 {
				t.Fatal("ordinary upload failure triggered an automatic retry")
			}
		})
	}
}

func TestAttachmentCacheWaiterStopsAfterSecondOwnerCancellation(t *testing.T) {
	cache := newAttachmentCache()
	key := sha256.Sum256([]byte("fixture"))
	first := &pendingImage{done: make(chan struct{}), err: context.Canceled}
	second := &pendingImage{done: make(chan struct{}), err: context.DeadlineExceeded}
	cache.pending[key] = first
	ctx := &countingWaitContext{Context: context.Background(), waiting: make(chan struct{}, 2)}
	result := make(chan error, 1)
	go func() {
		_, err := cache.get(ctx, key, func() (string, error) {
			t.Error("waiter started a third upload after two owner cancellations")
			return "file-unexpected", nil
		})
		result <- err
	}()
	<-ctx.waiting
	cache.mu.Lock()
	cache.pending[key] = second
	close(first.done)
	cache.mu.Unlock()
	<-ctx.waiting
	cache.mu.Lock()
	delete(cache.pending, key)
	close(second.done)
	cache.mu.Unlock()
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second owner cancellation returned %v", err)
	}
	if len(cache.entries) != 0 {
		t.Fatal("canceled uploads entered the cache")
	}
}

type countingWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (c *countingWaitContext) Done() <-chan struct{} {
	c.waiting <- struct{}{}
	return c.Context.Done()
}

type observedWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestAttachmentCacheBoundsCompletedAndPendingEntries(t *testing.T) {
	cache := newAttachmentCache()
	upload := func() (string, error) { return "file-fixture", nil }
	first := sha256.Sum256([]byte("entry-0"))
	for index := range maxCachedAttachments + 1 {
		key := sha256.Sum256([]byte(fmt.Sprintf("entry-%d", index)))
		if _, err := cache.get(context.Background(), key, upload); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != maxCachedAttachments || cache.entries[first] != nil {
		t.Fatal("attachment LRU did not evict its oldest entry")
	}
	for index := range maxPendingAttachments {
		key := sha256.Sum256([]byte(fmt.Sprintf("pending-%d", index)))
		cache.pending[key] = &pendingImage{done: make(chan struct{})}
	}
	_, err := cache.get(context.Background(), sha256.Sum256([]byte("overflow")), func() (string, error) {
		t.Error("pending limit allowed an extra upload")
		return "file-fixture", nil
	})
	if !errors.Is(err, ErrImageUpload) || len(cache.pending) != maxPendingAttachments {
		t.Fatal("pending upload bound was not enforced")
	}
}
