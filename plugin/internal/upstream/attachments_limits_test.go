package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func sizedImageFixture(t *testing.T, size, variant int) []byte {
	t.Helper()
	base := fixturePNG(t)
	if size < len(base)+16 {
		t.Fatal("image fixture too small")
	}
	text := bytes.Repeat([]byte{'x'}, size-len(base)-12)
	binary.BigEndian.PutUint32(text[:4], uint32(variant))
	chunk := make([]byte, len(text)+12)
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(text)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], text)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return append(append(append([]byte{}, base[:len(base)-12]...), chunk...), base[len(base)-12:]...)
}

func inlineImagePart(data []byte) map[string]string {
	return map[string]string{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}
}
func imageRequest(parts []any) []byte {
	raw, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": parts}}})
	return raw
}

func countingImageClient(t *testing.T, uploads *atomic.Int32) *Client {
	t.Helper()
	return NewWithTransport(accountTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.GetBody != nil {
			t.Error("upload method or replay protection changed")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			return nil, err
		}
		files := 0
		for {
			p, e := reader.NextPart()
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			if p.FormName() == "file" {
				files++
				if p.Header.Get("Content-Type") != "image/png" {
					t.Error("image MIME changed")
				}
			}
			if _, e = io.Copy(io.Discard, p); e != nil {
				return nil, e
			}
		}
		if files != 1 {
			t.Error("upload did not contain one image")
		}
		id := fmt.Sprintf("file-budget-%d", uploads.Add(1))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q,"openai_file_id":%q}`, id, id)))}, nil
	}))
}

func countImageReferences(t *testing.T, body []byte) (int, map[string]bool) {
	t.Helper()
	var input struct{ Input []map[string]json.RawMessage }
	if json.Unmarshal(body, &input) != nil {
		t.Fatal("invalid rewritten JSON")
	}
	count := 0
	ids := map[string]bool{}
	for _, item := range input.Input {
		for _, field := range []string{"content", "output"} {
			var parts []map[string]any
			if json.Unmarshal(item[field], &parts) != nil {
				continue
			}
			for _, p := range parts {
				if p["type"] == "input_image" {
					count++
					id, ok := p["file_id"].(string)
					if !ok || p["image_url"] != nil {
						t.Fatal("inline image not rewritten")
					}
					ids[id] = true
				}
			}
		}
	}
	return count, ids
}

func TestBPSRealHistoryShapePreservesAll23ImagesAcrossCacheAndAccountSwitch(t *testing.T) {
	const total = 5190392
	input := make([]any, 0, 94)
	for n := range 78 {
		input = append(input, map[string]any{"role": []string{"user", "assistant"}[n%2], "content": fmt.Sprintf("Synthetic history %d", n)})
	}
	sizes := []int{327441}
	remaining := total - sizes[0]
	for n := range 22 {
		size := remaining / (22 - n)
		sizes = append(sizes, size)
		remaining -= size
	}
	index := 0
	for n := range 8 {
		id := fmt.Sprintf("call-image-%d", n)
		input = append(input, map[string]any{"type": "custom_tool_call", "call_id": id, "name": "fixture_screenshot", "input": "synthetic"})
		parts := []any{map[string]string{"type": "input_text", "text": fmt.Sprintf("Result %d", n)}}
		if n < 6 {
			count := 4
			if n == 5 {
				count = 3
			}
			for range count {
				parts = append(parts, inlineImagePart(sizedImageFixture(t, sizes[index], index)))
				index++
			}
		}
		input = append(input, map[string]any{"type": "custom_tool_call_output", "call_id": id, "output": parts})
	}
	if len(input) != 94 || index != 23 {
		t.Fatal("real-history-shape fixture mismatch")
	}
	body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": input, "stream": true})
	before := bytes.Clone(body)
	var uploads atomic.Int32
	c := countingImageClient(t, &uploads)
	defer c.CloseIdleConnections()
	token := t.TempDir()
	for n, account := range []string{token, token, token + "-other-account"} {
		got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", account, body)
		if err != nil {
			t.Fatal(err)
		}
		count, ids := countImageReferences(t, got)
		if count != 23 || len(ids) != 23 {
			t.Fatal("image/history position lost", count, len(ids))
		}
		want := int32(23)
		if n == 2 {
			want = 46
		}
		if uploads.Load() != want {
			t.Fatalf("cache/account isolation uploads=%d want=%d", uploads.Load(), want)
		}
		var decoded struct{ Input []map[string]any }
		_ = json.Unmarshal(got, &decoded)
		if len(decoded.Input) != 94 || decoded.Input[79]["call_id"] != "call-image-0" || decoded.Input[93]["call_id"] != "call-image-7" {
			t.Fatal("tool history correlation changed")
		}
	}
	if !bytes.Equal(body, before) {
		t.Fatal("caller-owned history mutated")
	}
}

func TestBPSInlineReferenceCountsAndRequestLocalDedup(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		count, unique int
	}{{"nine", 9, 9}, {"fifty", 50, 50}, {"hundred", 100, 100}, {"hundred_repeated", 100, 1}, {"repeated1024", 1024, 1}, {"more_than_global_cache", 1024, 513}} {
		t.Run(scenario.name, func(t *testing.T) {
			parts := make([]any, scenario.count)
			data := make([]map[string]string, scenario.unique)
			for i := range data {
				data[i] = inlineImagePart(sizedImageFixture(t, 128, i))
			}
			for i := range parts {
				parts[i] = data[i%len(data)]
			}
			var uploads atomic.Int32
			c := countingImageClient(t, &uploads)
			defer c.CloseIdleConnections()
			token := t.TempDir()
			request := imageRequest(parts)
			got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", token, request)
			if err != nil {
				t.Fatal(err)
			}
			count, ids := countImageReferences(t, got)
			if count != scenario.count || len(ids) != scenario.unique || uploads.Load() != int32(scenario.unique) {
				t.Fatalf("dedup dropped refs or repeated uploads: refs=%d ids=%d uploads=%d", count, len(ids), uploads.Load())
			}
			if scenario.unique <= maxCachedAttachments {
				hot, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", token, request)
				if err != nil || !bytes.Equal(got, hot) || uploads.Load() != int32(scenario.unique) {
					t.Fatalf("hot cache changed history or reuploaded images: %v uploads=%d", err, uploads.Load())
				}
			}
		})
	}
}

func TestBPSPreflightRejectsLastBadImageWithoutAnyUpload(t *testing.T) {
	good := inlineImagePart(sizedImageFixture(t, 128, 1))
	for _, bad := range []map[string]string{{"type": "input_image", "image_url": "data:image/png;base64,not-valid"}, {"type": "input_image", "image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(sizedImageFixture(t, 128, 2))}} {
		parts := make([]any, 23)
		for i := range parts {
			parts[i] = good
		}
		parts = append(parts, bad)
		var uploads atomic.Int32
		c := countingImageClient(t, &uploads)
		got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", t.TempDir(), imageRequest(parts))
		c.CloseIdleConnections()
		if got != nil || !errors.Is(err, ErrInvalidInlineImage) || uploads.Load() != 0 {
			t.Fatalf("late invalid image caused side effects: %v uploads=%d", err, uploads.Load())
		}
	}
}

func TestBPSInlineReferenceOverflowFailsBeforeAnyUpload(t *testing.T) {
	part := inlineImagePart(sizedImageFixture(t, 128, 1))
	parts := make([]any, maxBPSInlineImages+1)
	for i := range parts {
		parts[i] = part
	}
	var uploads atomic.Int32
	c := countingImageClient(t, &uploads)
	defer c.CloseIdleConnections()
	got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", t.TempDir(), imageRequest(parts))
	var problem *ImageLimitError
	if got != nil || !errors.As(err, &problem) || problem.Kind != "reference_count" || problem.Limit != 1024 || problem.Actual != 1025 || uploads.Load() != 0 {
		t.Fatalf("reference overflow reached upload or lost diagnosis: %v uploads=%d", err, uploads.Load())
	}
}

func TestBPSImageBudgetBoundariesAndSafeNumericDiagnostics(t *testing.T) {
	if maxBPSInlineImages != 1024 || maxBPSInlineImageBytes != 20<<20 || maxBPSInlineUniqueBytes != 64<<20 || maxBPSInlineEncodedBytes != 96<<20 {
		t.Fatal("BPS image budgets changed unexpectedly")
	}
	for _, test := range []struct {
		kind  string
		limit int
		run   func(int) error
	}{
		{"encoded_bytes", maxBPSInlineEncodedBytes, func(n int) error { b := inlineImageBudget{bps: true}; return b.addReference(n) }},
		{"unique_bytes", maxBPSInlineUniqueBytes, func(n int) error { b := inlineImageBudget{bps: true}; return b.addDecoded(n, true) }},
		{"total_bytes", maxInlineImageTotal, func(n int) error { b := inlineImageBudget{}; return b.addDecoded(n, true) }},
	} {
		for _, offset := range []int{-1, 0, 1} {
			err := test.run(test.limit + offset)
			if offset <= 0 {
				if err != nil {
					t.Fatal(test.kind, offset, err)
				}
				continue
			}
			var problem *ImageLimitError
			if !errors.As(err, &problem) || !errors.Is(err, ErrInlineImageLimit) || problem.Kind != test.kind || problem.Limit != int64(test.limit) || problem.Actual != int64(test.limit+1) {
				t.Fatalf("wrong safe diagnostic: %#v", err)
			}
			if strings.Contains(problem.Error(), "data:") {
				t.Fatal("image URI leaked")
			}
		}
	}
	b := inlineImageBudget{bps: true}
	if err := b.addDecoded(maxBPSInlineUniqueBytes, true); err != nil {
		t.Fatal(err)
	}
	if err := b.addDecoded(maxBPSInlineUniqueBytes, false); err != nil || b.total != maxBPSInlineUniqueBytes {
		t.Fatal("duplicate image used unique-byte budget")
	}
	for range maxBPSInlineImages {
		if err := b.addReference(1); err != nil {
			t.Fatal(err)
		}
	}
	var problem *ImageLimitError
	if err := b.addReference(1); !errors.As(err, &problem) || problem.Kind != "reference_count" || problem.Actual != 1025 {
		t.Fatal("reference limit failed", err)
	}
	// Encoded budget counts repeats even when their decoded bytes deduplicate.
	b = inlineImageBudget{bps: true}
	if err := b.addReference(maxBPSInlineEncodedBytes / 2); err != nil {
		t.Fatal(err)
	}
	if err := b.addReference(maxBPSInlineEncodedBytes / 2); err != nil {
		t.Fatal(err)
	}
	if err := b.addReference(1); !errors.Is(err, ErrInlineImageLimit) {
		t.Fatal("repeated raw encodings bypassed the request budget")
	}
}

func TestBPSImageSingleByteLimitAndStrictBase64(t *testing.T) {
	for _, size := range []int{maxBPSInlineImageBytes - 1, maxBPSInlineImageBytes, maxBPSInlineImageBytes + 1} {
		data := sizedImageFixture(t, size, size)
		content, err := parseInlineImageSource("DATA:IMAGE/PNG;BASE64," + base64.StdEncoding.EncodeToString(data))
		if err != nil {
			t.Fatal(err)
		}
		err = inspectInlineImage(context.Background(), content, maxBPSInlineImageBytes)
		if size <= maxBPSInlineImageBytes {
			if err != nil || content.bytes != size || content.digest != sha256.Sum256(data) {
				t.Fatalf("legal boundary rejected: size=%d %v", size, err)
			}
		} else {
			var problem *ImageLimitError
			if !errors.As(err, &problem) || problem.Kind != "image_bytes" || problem.Actual != int64(size) {
				t.Fatalf("oversize boundary accepted: %v", err)
			}
		}
	}
	for _, size := range []int{127, 128, 129} {
		data := sizedImageFixture(t, size, size)
		encoded := base64.StdEncoding.EncodeToString(data)
		wrapped := strings.Join(strings.Split(encoded, ""), "\r\n")
		content, err := parseInlineImageSource("dAtA:iMaGe/PnG;BaSe64," + wrapped)
		if err != nil {
			t.Fatal(err)
		}
		if err := inspectInlineImage(context.Background(), content, size); err != nil || content.bytes != size || content.digest != sha256.Sum256(data) {
			t.Fatalf("CRLF/padding changed actual byte count: %d %v", size, err)
		}
	}
	encoded := base64.StdEncoding.EncodeToString(sizedImageFixture(t, 128, 7))
	for _, bad := range []string{encoded + "AAAA", encoded[:len(encoded)-1], " " + encoded, strings.TrimRight(encoded, "=") + "!="} {
		content, _ := parseInlineImageSource("data:image/png;base64," + bad)
		if err := inspectInlineImage(context.Background(), content, maxBPSInlineImageBytes); !errors.Is(err, ErrInvalidInlineImage) {
			t.Fatalf("invalid base64 accepted: %v", err)
		}
	}
}

func TestBPSPreflightSharesEquivalentEncodingsAndGenericKeepsLimits(t *testing.T) {
	data := sizedImageFixture(t, 128, 1)
	encoded := base64.StdEncoding.EncodeToString(data)
	parts := []any{inlineImagePart(data), map[string]string{"type": "input_image", "image_url": "DATA:IMAGE/PNG;BASE64," + encoded[:40] + "\r\n" + encoded[40:]}}
	var body struct{ Input []json.RawMessage }
	_ = json.Unmarshal(imageRequest(parts), &body)
	rewrites, err := inspectInlineImages(context.Background(), body.Input, true)
	if err != nil || rewrites[0].images[0].content != rewrites[0].images[1].content {
		t.Fatal("decoded identity did not share canonical encoded source", err)
	}
	for len(parts) < 9 {
		parts = append(parts, inlineImagePart(data))
	}
	var uploads atomic.Int32
	c := countingImageClient(t, &uploads)
	defer c.CloseIdleConnections()
	if _, err := c.RewriteInlineImages(context.Background(), "https://fixture.invalid/files", t.TempDir(), imageRequest(parts)); !errors.Is(err, ErrInlineImageLimit) || uploads.Load() != 0 {
		t.Fatal("generic Files count limit changed", err)
	}
	plain := []byte(`{"input":"data:image/png;base64,` + strings.Repeat("x", 1<<20) + `"}`)
	got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", t.TempDir(), plain)
	if err != nil || !bytes.Equal(got, plain) || uploads.Load() != 0 {
		t.Fatal("plain text was counted as image data", err)
	}
}

func TestBPSLargeHistoryConcurrentIdenticalImageUsesOneUpload(t *testing.T) {
	parts := make([]any, 23)
	part := inlineImagePart(sizedImageFixture(t, 128, 1))
	for i := range parts {
		parts[i] = part
	}
	body := imageRequest(parts)
	var uploads atomic.Int32
	c := countingImageClient(t, &uploads)
	defer c.CloseIdleConnections()
	token := t.TempDir()
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			got, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", token, body)
			if err != nil {
				t.Error(err)
				return
			}
			count, _ := countImageReferences(t, got)
			if count != 23 {
				t.Error("concurrent image refs lost")
			}
		})
	}
	group.Wait()
	if uploads.Load() != 1 {
		t.Fatalf("concurrent duplicate uploads=%d", uploads.Load())
	}
}

func TestBPSImagePipelineCanceledOwnerHandsOffAndReleasesCache(t *testing.T) {
	parts := make([]any, 23)
	part := inlineImagePart(sizedImageFixture(t, 128, 1))
	for i := range parts {
		parts[i] = part
	}
	payload := imageRequest(parts)
	owner, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	started, replaced, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := NewWithTransport(accountTransport(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		case 2:
			close(replaced)
			<-release
		default:
			t.Error("canceled image owner caused duplicate replacement upload")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai_file_id":"file-live-replacement"}`))}, nil
	}))
	defer c.CloseIdleConnections()
	token := t.TempDir()
	ownerDone := make(chan error, 1)
	go func() {
		_, err := c.RewriteBPSImages(owner, "https://fixture.invalid/attachments", token, payload)
		ownerDone <- err
	}()
	<-started
	var waiters sync.WaitGroup
	for range 20 {
		ctx := &observedWaitContext{Context: context.Background(), waiting: make(chan struct{})}
		waiters.Go(func() {
			got, err := c.RewriteBPSImages(ctx, "https://fixture.invalid/attachments", token, payload)
			if err != nil {
				t.Error(err)
				return
			}
			count, ids := countImageReferences(t, got)
			if count != 23 || !ids["file-live-replacement"] {
				t.Error("live waiter lost image history")
			}
		})
		<-ctx.waiting
	}
	cancelOwner()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation lost: %v", err)
	}
	<-replaced
	close(release)
	waiters.Wait()
	if calls.Load() != 2 {
		t.Fatalf("canceled owner did not deduplicate replacement: %d", calls.Load())
	}
	if _, err := c.RewriteBPSImages(context.Background(), "https://fixture.invalid/attachments", token, payload); err != nil || calls.Load() != 2 {
		t.Fatal("replacement image not cached", err)
	}
}

func FuzzInlineImageStreamingDecoderMatchesStrictDecode(f *testing.F) {
	f.Add("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aAq0AAAAASUVORK5CYII=")
	f.Add("aGVsbG8=")
	f.Add("YQ==\r\n")
	f.Add("YR==")
	f.Fuzz(func(t *testing.T, encoded string) {
		if len(encoded) > 8192 {
			return
		}
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
		content := &inlineImageContent{mimeType: "image/png", encoded: encoded}
		err := inspectInlineImage(context.Background(), content, 8192)
		valid := decodeErr == nil && http.DetectContentType(decoded) == "image/png"
		if (err == nil) != valid {
			t.Fatalf("stream decoder disagrees with strict decoder: valid=%v err=%v", valid, err)
		}
		if valid && (content.bytes != len(decoded) || content.digest != sha256.Sum256(decoded)) {
			t.Fatal("stream size/digest differs from decoded content")
		}
	})
}
