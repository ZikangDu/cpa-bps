package upstream

import (
	"bytes"
	"context"
	"sync"
)

const (
	maxBPSUploadWorkers = 4
	maxImageUploads     = 8
	maxImageUploadBytes = 64 << 20
)

var imageUploads = newImageUploadLimiter()

type imageUploadLimiter struct {
	mu            sync.Mutex
	active, bytes int
	changed       chan struct{}
}

func newImageUploadLimiter() *imageUploadLimiter {
	return &imageUploadLimiter{changed: make(chan struct{})}
}

func (l *imageUploadLimiter) acquire(ctx context.Context, size int) (func(), error) {
	if size < 1 || size > maxImageUploadBytes {
		return nil, ErrImageUpload
	}
	for {
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		if l.active < maxImageUploads && size <= maxImageUploadBytes-l.bytes {
			l.active++
			l.bytes += size
			l.mu.Unlock()
			return sync.OnceFunc(func() {
				l.mu.Lock()
				l.active--
				l.bytes -= size
				close(l.changed)
				l.changed = make(chan struct{})
				l.mu.Unlock()
			}), nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

type imageMultipartWriter struct {
	body      *bytes.Buffer
	remaining int
}

func (w *imageMultipartWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		return 0, ErrImageUpload
	}
	n, err := w.body.Write(data)
	w.remaining -= n
	return n, err
}

func (c *Client) uploadInlineImages(ctx context.Context, endpoint, token string, rewrites []attachmentRewrite, bps bool) (map[*inlineImageContent]string, error) {
	// The canonical content pointers are created only after full preflight.
	// Keep request-local references even if the shared LRU evicts an entry.
	unique := make([]inlineImage, 0)
	seen := make(map[*inlineImageContent]bool)
	for _, rewrite := range rewrites {
		for _, image := range rewrite.images {
			if !seen[image.content] {
				seen[image.content] = true
				unique = append(unique, image)
			}
		}
	}
	workers := 1
	if bps {
		workers = min(maxBPSUploadWorkers, len(unique))
	}
	batch, cancel := context.WithCancel(ctx)
	defer cancel()
	ids := make([]string, len(unique))
	var mu sync.Mutex
	next := 0
	var firstErr error
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for {
				mu.Lock()
				if firstErr != nil || batch.Err() != nil || next >= len(unique) {
					mu.Unlock()
					return
				}
				index := next
				next++
				mu.Unlock()
				image := unique[index]
				id, err := uploadedImages.get(batch, attachmentCacheKey(endpoint, token, image, bps), func() (string, error) { return c.uploadImage(batch, endpoint, token, image, bps) })
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					return
				}
				ids[index] = id
			}
		})
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, firstErr
	}
	result := make(map[*inlineImageContent]string, len(unique))
	for index, image := range unique {
		result[image.content] = ids[index]
	}
	return result, nil
}
