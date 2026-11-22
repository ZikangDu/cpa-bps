package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func awaitImageSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("bounded upload barrier timed out")
	}
}

func TestImageUploadLimiterEnforcesCountBytesAndCancelableWait(t *testing.T) {
	for _, byteLimited := range []bool{false, true} {
		t.Run(fmt.Sprint(byteLimited), func(t *testing.T) {
			limiter := newImageUploadLimiter()
			count, size := maxImageUploads, 1
			if byteLimited {
				count, size = 3, 20<<20
			}
			releases := make([]func(), count)
			for i := range count {
				var err error
				releases[i], err = limiter.acquire(context.Background(), size)
				if err != nil {
					t.Fatal(err)
				}
			}
			base, cancel := context.WithCancel(context.Background())
			ctx := &observedWaitContext{Context: base, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				release, err := limiter.acquire(ctx, size)
				if err == nil {
					release()
				}
				done <- err
			}()
			awaitImageSignal(t, ctx.waiting)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("capacity wait lost cancellation", err)
			}
			if limiter.active != count || limiter.bytes != count*size {
				t.Fatal("canceled wait changed permits")
			}
			for _, release := range releases {
				release()
				release()
			}
			if limiter.active != 0 || limiter.bytes != 0 {
				t.Fatal("permit release leaked or doubled")
			}
		})
	}
}

func TestBPSImageUploadHasFourWorkersAndGenericStaysSerial(t *testing.T) {
	for _, bps := range []bool{false, true} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			parts := make([]any, 8)
			for i := range parts {
				parts[i] = inlineImagePart(sizedImageFixture(t, 128, i))
			}
			var active, peak, posts atomic.Int32
			entered := make(chan struct{}, 8)
			release := make(chan struct{})
			c := NewWithTransport(accountTransport(func(req *http.Request) (*http.Response, error) {
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				defer active.Add(-1)
				number := posts.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":"file-%d","openai_file_id":"file-%d"}`, number, number)))}, nil
			}))
			defer c.CloseIdleConnections()
			done := make(chan error, 1)
			token := t.TempDir()
			go func() {
				_, err := c.rewriteImages(context.Background(), "https://fixture.invalid/attachments", token, imageRequest(parts), bps)
				done <- err
			}()
			want := 1
			if bps {
				want = 4
			}
			for range want {
				awaitImageSignal(t, entered)
			}
			if active.Load() != int32(want) {
				t.Fatal("wrong per-request parallelism")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if peak.Load() != int32(want) || posts.Load() != 8 {
				t.Fatalf("parallelism=%d uploads=%d", peak.Load(), posts.Load())
			}
		})
	}
}

func TestBPS20RequestsDifferentImagesStayWithinGlobalUploadBudget(t *testing.T) {
	var active, peak, posts atomic.Int32
	entered := make(chan struct{}, 80)
	release := make(chan struct{})
	c := NewWithTransport(accountTransport(func(req *http.Request) (*http.Response, error) {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		defer active.Add(-1)
		number := posts.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"openai_file_id":"file-global-%d"}`, number)))}, nil
	}))
	defer c.CloseIdleConnections()
	token := t.TempDir()
	var group sync.WaitGroup
	for request := range 20 {
		parts := make([]any, 4)
		for i := range parts {
			parts[i] = inlineImagePart(sizedImageFixture(t, 128, request*4+i))
		}
		payload := imageRequest(parts)
		group.Go(func() {
			got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", token, payload)
			if err != nil {
				t.Error(err)
				return
			}
			count, _ := countImageReferences(t, got)
			if count != 4 {
				t.Error("parallel images lost")
			}
		})
	}
	for range maxImageUploads {
		awaitImageSignal(t, entered)
	}
	imageUploads.mu.Lock()
	count, allocated := imageUploads.active, imageUploads.bytes
	imageUploads.mu.Unlock()
	if count != 8 || allocated > maxImageUploadBytes {
		t.Fatal("global upload resource bound violated", count, allocated)
	}
	close(release)
	group.Wait()
	if peak.Load() != 8 || posts.Load() != 80 {
		t.Fatalf("global bound or capacity wait failed: peak=%d posts=%d", peak.Load(), posts.Load())
	}
	imageUploads.mu.Lock()
	defer imageUploads.mu.Unlock()
	if imageUploads.active != 0 || imageUploads.bytes != 0 {
		t.Fatal("global upload resources leaked")
	}
}

func TestBPSParallelUploadFailureCancelsBatchAndNeverReturnsPartialPayload(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelParent), func(t *testing.T) {
			parts := make([]any, 20)
			for i := range parts {
				parts[i] = inlineImagePart(sizedImageFixture(t, 128, i))
			}
			payload := imageRequest(parts)
			before := bytes.Clone(payload)
			entered := make(chan struct{}, 4)
			trigger := make(chan struct{})
			var posts, active atomic.Int32
			c := NewWithTransport(accountTransport(func(req *http.Request) (*http.Response, error) {
				active.Add(1)
				defer active.Add(-1)
				number := posts.Add(1)
				entered <- struct{}{}
				if number == 1 && !cancelParent {
					<-trigger
					return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				<-req.Context().Done()
				return nil, req.Context().Err()
			}))
			defer c.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			token := t.TempDir()
			go func() {
				got, err := c.RewriteBPSImages(ctx, "https://fixture.invalid/attachments", token, payload)
				if got != nil {
					t.Error("partial upload batch produced a generation payload")
				}
				done <- err
			}()
			for range 4 {
				awaitImageSignal(t, entered)
			}
			if cancelParent {
				cancel()
			} else {
				close(trigger)
			}
			select {
			case err := <-done:
				want := ErrImageUpload
				if cancelParent {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatal("batch lost original failure", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("failed/canceled batch retained worker")
			}
			if active.Load() != 0 || posts.Load() != 4 || !bytes.Equal(payload, before) {
				t.Fatal("partial failure dispatched more images, retained active work or mutated input")
			}
			imageUploads.mu.Lock()
			count, size := imageUploads.active, imageUploads.bytes
			imageUploads.mu.Unlock()
			if count != 0 || size != 0 {
				t.Fatal("canceled batch retained global permit")
			}
		})
	}
}
