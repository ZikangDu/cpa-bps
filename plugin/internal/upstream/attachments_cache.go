package upstream

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

const (
	maxCachedAttachments  = 512
	maxPendingAttachments = 64
	// One canceled owner can be replaced by a still-live waiter. A second
	// canceled owner ends that waiter's attempt instead of chaining forever.
	maxAttachmentOwnerHandoffs = 1
	// Files uploads request a one-hour lifetime. BPS does not supply a lifetime,
	// so references are reused only during a short, fixed multi-turn window.
	attachmentCacheTTL = 5 * time.Minute
)

var uploadedImages = newAttachmentCache()

type cachedImage struct {
	key     [sha256.Size]byte
	fileID  string
	expires time.Time
}

type pendingImage struct {
	done   chan struct{}
	fileID string
	err    error
}

// attachmentCache retains only credential/content digests and returned file
// identifiers. Original images and bearer credentials are never stored here.
type attachmentCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*list.Element
	order   list.List
	pending map[[sha256.Size]byte]*pendingImage
	now     func() time.Time
}

func newAttachmentCache() *attachmentCache {
	return &attachmentCache{
		entries: make(map[[sha256.Size]byte]*list.Element),
		pending: make(map[[sha256.Size]byte]*pendingImage),
		now:     time.Now,
	}
}

func attachmentCacheKey(endpoint, token string, image inlineImage, bps bool) [sha256.Size]byte {
	hash := sha256.New()
	protocol := "files"
	if bps {
		protocol = "bps"
	}
	for _, field := range []string{"attachment-digest-v2", endpoint, token, image.content.mimeType, protocol} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	_, _ = hash.Write(image.content.digest[:])
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	return key
}

func (c *attachmentCache) get(ctx context.Context, key [sha256.Size]byte, upload func() (string, error)) (string, error) {
	handoffs := 0
	for {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return "", err
		}
		now := c.now()
		if entry := c.entries[key]; entry != nil {
			cached := entry.Value.(cachedImage)
			if now.Before(cached.expires) {
				c.order.MoveToFront(entry)
				c.mu.Unlock()
				return cached.fileID, nil
			}
			delete(c.entries, key)
			c.order.Remove(entry)
		}
		if existing := c.pending[key]; existing != nil {
			c.mu.Unlock()
			select {
			case <-existing.done:
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if handoffs < maxAttachmentOwnerHandoffs &&
					(errors.Is(existing.err, context.Canceled) || errors.Is(existing.err, context.DeadlineExceeded)) {
					handoffs++
					continue
				}
				return existing.fileID, existing.err
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if len(c.pending) >= maxPendingAttachments {
			c.mu.Unlock()
			return "", ErrImageUpload
		}
		pending := &pendingImage{done: make(chan struct{})}
		c.pending[key] = pending
		c.mu.Unlock()

		fileID, err := upload()
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.pending, key)
		if ownerErr := ctx.Err(); ownerErr != nil {
			// An upload can complete concurrently with cancellation. Do not publish
			// its reference to other callers or retain it after the owner stops.
			fileID, err = "", ownerErr
		} else if err != nil {
			fileID = ""
		}
		expires := now.Add(attachmentCacheTTL)
		if err == nil && validImageFileID(fileID) && c.now().Before(expires) {
			c.entries[key] = c.order.PushFront(cachedImage{key: key, fileID: fileID, expires: expires})
			if c.order.Len() > maxCachedAttachments {
				oldest := c.order.Back()
				delete(c.entries, oldest.Value.(cachedImage).key)
				c.order.Remove(oldest)
			}
		}
		pending.fileID, pending.err = fileID, err
		close(pending.done)
		return fileID, err
	}
}
