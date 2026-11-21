package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cpa-bps-reconstruction/internal/documents"
)

const documentProcessWindow = 45 * time.Second
const documentCacheTTL = 5 * time.Minute
const documentCacheBytes = 64 << 20

type documentFlight struct {
	done    chan struct{}
	result  documents.Result
	err     error
	expires time.Time
	bytes   int
}
type documentProcesses struct {
	mu      sync.Mutex
	entries map[[32]byte]*documentFlight
	slots   chan struct{}
	size    int
}

func newDocumentProcesses() *documentProcesses {
	return &documentProcesses{entries: map[[32]byte]*documentFlight{}, slots: make(chan struct{}, 2)}
}

type documentOutput struct {
	bytes.Buffer
	limit int
}

func (b *documentOutput) Write(raw []byte) (int, error) {
	if len(raw) > b.limit-b.Len() {
		return 0, errors.New("document_output_limit")
	}
	return b.Buffer.Write(raw)
}

func (p *documentProcesses) parse(ctx context.Context, worker, python, scope string, job documents.Job) (documents.Result, error) {
	// No OAuth credential participates in this key: an account failover must not
	// parse the same user's immutable file again. Callers remain isolated.
	digest := sha256.Sum256(job.Data)
	identity, _ := json.Marshal([]any{"doc-v1", scope, job.Filename, digest, worker, python})
	key := sha256.Sum256(identity)
	p.mu.Lock()
	now := time.Now()
	for k, v := range p.entries {
		select {
		case <-v.done:
			if !now.Before(v.expires) {
				delete(p.entries, k)
				p.size -= v.bytes
			}
		default:
		}
	}
	if old := p.entries[key]; old != nil {
		p.mu.Unlock()
		select {
		case <-old.done:
			return old.result, old.err
		case <-ctx.Done():
			return documents.Result{}, ctx.Err()
		}
	}
	if len(p.entries) >= 16 {
		var oldestKey [32]byte
		var oldest *documentFlight
		for k, v := range p.entries {
			select {
			case <-v.done:
				if oldest == nil || v.expires.Before(oldest.expires) {
					oldestKey, oldest = k, v
				}
			default:
			}
		}
		if oldest != nil {
			delete(p.entries, oldestKey)
			p.size -= oldest.bytes
		}
	}
	if len(p.entries) >= 16 {
		p.mu.Unlock()
		return documents.Result{}, errors.New("document_parser_busy")
	}
	next := &documentFlight{done: make(chan struct{})}
	p.entries[key] = next
	p.mu.Unlock()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		next.err = ctx.Err()
	}
	if next.err == nil {
		next.result, next.err = runDocumentWorker(ctx, worker, python, job)
		<-p.slots
	}
	next.expires = time.Now().Add(documentCacheTTL)
	if next.err != nil {
		next.expires = time.Now().Add(5 * time.Second)
	}
	next.bytes = len(next.result.Text)
	for _, image := range next.result.Images {
		next.bytes += len(image.PNG)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	close(next.done)
	if next.err == nil && next.bytes <= documentCacheBytes {
		for p.size+next.bytes > documentCacheBytes {
			var oldestKey [32]byte
			var oldest *documentFlight
			for k, v := range p.entries {
				if v == next {
					continue
				}
				select {
				case <-v.done:
					if oldest == nil || v.expires.Before(oldest.expires) {
						oldestKey, oldest = k, v
					}
				default:
				}
			}
			if oldest == nil {
				break
			}
			delete(p.entries, oldestKey)
			p.size -= oldest.bytes
		}
		p.size += next.bytes
	} else if next.err == nil {
		delete(p.entries, key)
	}
	return next.result, next.err
}

func runDocumentWorker(ctx context.Context, worker, python string, job documents.Job) (documents.Result, error) {
	executable, err := exec.LookPath(worker)
	if err != nil {
		return documents.Result{}, errors.New("document_parser_unavailable")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return documents.Result{}, errors.New("document_parser_unavailable")
	}
	if strings.ContainsAny(python, "/\\") && !filepath.IsAbs(python) {
		python, _ = filepath.Abs(python)
	}
	directory, err := os.MkdirTemp("", "cpa-bps-document-job-")
	if err != nil {
		return documents.Result{}, errors.New("document_parser_unavailable")
	}
	defer os.RemoveAll(directory)
	readCtx, cancel := context.WithTimeout(ctx, documentProcessWindow)
	defer cancel()
	job.Python = python
	encoded, err := json.Marshal(job)
	if err != nil {
		return documents.Result{}, errors.New("invalid_document_input")
	}
	command := exec.CommandContext(readCtx, executable)
	configureDocumentProcess(command)
	command.Dir = directory
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + directory, "LANG=C.UTF-8", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1"}
	command.Stdin = bytes.NewReader(encoded)
	stdout := &documentOutput{limit: documents.MaxResultBytes}
	stderr := &documentOutput{limit: 4096}
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		if readCtx.Err() != nil {
			return documents.Result{}, errors.New("document_processing_timeout")
		}
		return documents.Result{}, errors.New("document_parser_failed")
	}
	var reply documents.Reply
	if json.Unmarshal(stdout.Bytes(), &reply) != nil {
		return documents.Result{}, errors.New("document_parser_failed")
	}
	if reply.Error != "" {
		return documents.Result{}, errors.New(reply.Error)
	}
	if reply.Result == nil || len(reply.Result.Text) > documents.MaxTextBytes || len(reply.Result.Images) > 8 {
		return documents.Result{}, errors.New("document_output_limit")
	}
	size := 0
	for _, image := range reply.Result.Images {
		size += len(image.PNG)
		if len(image.PNG) > 5<<20 {
			return documents.Result{}, errors.New("document_visual_limit")
		}
	}
	if size > 16<<20 {
		return documents.Result{}, errors.New("document_visual_limit")
	}
	return *reply.Result, nil
}
