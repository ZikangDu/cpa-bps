package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

const (
	maxInlineImages          = 8
	maxInlineImageBytes      = 5 << 20
	maxInlineImageTotal      = 16 << 20
	maxUploadReplyBytes      = 64 << 10
	maxBPSInlineImages       = 1024
	maxBPSInlineImageBytes   = 20 << 20
	maxBPSInlineUniqueBytes  = 64 << 20
	maxBPSInlineEncodedBytes = 96 << 20
)

// ImageLimitError reports only a fixed budget category and numeric values.
// No filename, image URL, content digest or request text crosses this boundary.
type ImageLimitError struct {
	Kind   string
	Limit  int64
	Actual int64
}

func (e *ImageLimitError) Error() string {
	return fmt.Sprintf("inline image limit exceeded: %s (limit=%d, actual=%d)", e.Kind, e.Limit, e.Actual)
}
func (e *ImageLimitError) Unwrap() error { return ErrInlineImageLimit }

func imageLimit(kind string, limit, actual int) error {
	return &ImageLimitError{Kind: kind, Limit: int64(limit), Actual: int64(actual)}
}

var (
	ErrInvalidInlineImage = errors.New("inline image is not a supported base64 image")
	ErrInlineImageLimit   = errors.New("inline images exceed the attachment limits")
	ErrImageUpload        = errors.New("upstream image upload failed")
	ErrUnsupportedBPSFile = errors.New("BPS does not support document input_file content")
)

type attachmentRewrite struct {
	itemIndex int
	field     string
	item      map[string]json.RawMessage
	parts     []json.RawMessage
	images    []inlineImage
}

type inlineImage struct {
	partIndex int
	part      map[string]json.RawMessage
	content   *inlineImageContent
}

type inlineImageContent struct {
	mimeType  string
	extension string
	encoded   string
	bytes     int
	digest    [sha256.Size]byte
}

// RewriteInlineImages optionally uploads normalized Responses image data URLs
// through the configured Files endpoint. The caller must scope filesURL and
// token to the Responses origin. The upstream expiry also covers partial uploads.
func (c *Client) RewriteInlineImages(ctx context.Context, filesURL, token string, payload []byte) ([]byte, error) {
	return c.rewriteImages(ctx, filesURL, token, payload, false)
}

// BPS attachments use a same-origin multipart endpoint returning openai_file_id.
func (c *Client) RewriteBPSImages(ctx context.Context, attachmentURL, token string, payload []byte) ([]byte, error) {
	return c.rewriteImages(ctx, attachmentURL, token, payload, true)
}

func (c *Client) rewriteImages(ctx context.Context, filesURL, token string, payload []byte, bps bool) ([]byte, error) {
	if filesURL == "" {
		return payload, nil
	}
	if err := ValidateEndpoint(filesURL); err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil || body == nil {
		return nil, ErrInvalidInlineImage
	}
	var input []json.RawMessage
	if err := json.Unmarshal(body["input"], &input); err != nil {
		return payload, nil // Plain text input needs no Files request.
	}
	canonicalized := false
	if bps {
		var err error
		canonicalized, err = normalizeBPSImageReferences(input)
		if err != nil {
			return nil, err
		}
	}
	rewrites, err := inspectInlineImages(ctx, input, bps)
	if err != nil {
		return nil, err
	}
	if len(rewrites) == 0 {
		if canonicalized {
			body["input"], _ = json.Marshal(input)
			return json.Marshal(body)
		}
		return payload, nil
	}
	// Preflight every image before creating any files. A failed upload is not
	// replayed, and the caller receives no partially rewritten request.
	fileIDs := make(map[*inlineImageContent]string)
	for _, rewrite := range rewrites {
		for _, image := range rewrite.images {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fileID := fileIDs[image.content]
			if fileID == "" {
				key := attachmentCacheKey(filesURL, token, image, bps)
				var errUpload error
				fileID, errUpload = uploadedImages.get(ctx, key, func() (string, error) {
					return c.uploadImage(ctx, filesURL, token, image, bps)
				})
				if errUpload != nil {
					return nil, errUpload
				}
				// Request-local reuse remains valid even if unrelated concurrent
				// uploads evict this reference from the global bounded cache.
				fileIDs[image.content] = fileID
			}
			if bps {
				// Native references do not accept public detail or extensions.
				image.part = bpsImageReference(fileID)
			} else {
				delete(image.part, "image_url")
				image.part["file_id"], _ = json.Marshal(fileID)
			}
			rewrite.parts[image.partIndex], _ = json.Marshal(image.part)
		}
		rewrite.item[rewrite.field], _ = json.Marshal(rewrite.parts)
		input[rewrite.itemIndex], _ = json.Marshal(rewrite.item)
	}
	body["input"], _ = json.Marshal(input)
	return json.Marshal(body)
}

func bpsImageReference(fileID string) map[string]json.RawMessage {
	encoded, _ := json.Marshal(fileID)
	return map[string]json.RawMessage{"type": json.RawMessage(`"input_image"`), "file_id": encoded}
}

// Validate the whole request before any upload. Existing and uploaded BPS
// references have the same two-field contract; remote URLs are not downloaded.
func normalizeBPSImageReferences(input []json.RawMessage) (bool, error) {
	changed := false
	for itemIndex, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		fields := []string{"content"}
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			fields = append(fields, "output")
		}
		itemChanged := false
		for _, field := range fields {
			var parts []json.RawMessage
			if json.Unmarshal(item[field], &parts) != nil {
				continue
			}
			partsChanged := false
			for index, rawPart := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(rawPart, &part) != nil || part == nil {
					continue
				}
				var partType string
				if json.Unmarshal(part["type"], &partType) != nil {
					continue
				}
				if partType == "input_file" {
					return false, ErrUnsupportedBPSFile
				}
				if partType != "input_image" {
					continue
				}
				var imageURL, fileID string
				for key, target := range map[string]*string{"image_url": &imageURL, "file_id": &fileID} {
					value := bytes.TrimSpace(part[key])
					if len(value) > 0 && !bytes.Equal(value, []byte("null")) && json.Unmarshal(value, target) != nil {
						return false, ErrInvalidInlineImage
					}
				}
				if imageURL != "" && fileID != "" || strings.TrimSpace(imageURL) == "" && strings.TrimSpace(fileID) == "" {
					return false, ErrInvalidInlineImage
				}
				if fileID != "" {
					if strings.TrimSpace(fileID) == "" {
						return false, ErrInvalidInlineImage
					}
					parts[index], _ = json.Marshal(bpsImageReference(fileID))
					partsChanged = true
				}
			}
			if partsChanged {
				item[field], _ = json.Marshal(parts)
				itemChanged = true
			}
		}
		if itemChanged {
			input[itemIndex], _ = json.Marshal(item)
			changed = true
		}
	}
	return changed, nil
}

type inlineImageBudget struct {
	bps                   bool
	count, total, encoded int
}

func (b *inlineImageBudget) addReference(encoded int) error {
	limit := maxInlineImages
	if b.bps {
		limit = maxBPSInlineImages
	}
	b.count++
	if b.count > limit {
		return imageLimit("reference_count", limit, b.count)
	}
	if b.bps {
		if encoded > maxBPSInlineEncodedBytes-b.encoded {
			return imageLimit("encoded_bytes", maxBPSInlineEncodedBytes, b.encoded+encoded)
		}
		b.encoded += encoded
	}
	return nil
}
func (b *inlineImageBudget) addDecoded(size int, unique bool) error {
	limit, kind := maxInlineImageTotal, "total_bytes"
	if b.bps {
		if !unique {
			return nil
		}
		limit, kind = maxBPSInlineUniqueBytes, "unique_bytes"
	}
	if size > limit-b.total {
		return imageLimit(kind, limit, b.total+size)
	}
	b.total += size
	return nil
}

func inspectInlineImages(ctx context.Context, input []json.RawMessage, bps bool) ([]attachmentRewrite, error) {
	var rewrites []attachmentRewrite
	budget := inlineImageBudget{bps: bps}
	byteLimit := maxInlineImageBytes
	if bps {
		byteLimit = maxBPSInlineImageBytes
	}
	type contentKey struct {
		mime   string
		digest [sha256.Size]byte
	}
	unique := make(map[contentKey]*inlineImageContent)
	// Exact duplicate encodings need neither repeated hashing nor decoding.
	// Equivalent encodings (case/CRLF) converge on the decoded content digest.
	type encodedKey struct {
		mime   string
		digest [sha256.Size]byte
	}
	encodings := make(map[encodedKey]*inlineImageContent)
	for itemIndex, rawItem := range input {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) != nil {
			continue
		}
		fields := []string{"content"}
		var itemType string
		_ = json.Unmarshal(item["type"], &itemType)
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			fields = append(fields, "output")
		}
		for _, field := range fields {
			var parts []json.RawMessage
			if json.Unmarshal(item[field], &parts) != nil {
				continue
			}
			rewrite := attachmentRewrite{itemIndex: itemIndex, field: field, item: item, parts: parts}
			for index, rawPart := range parts {
				part, source := inlineImageSource(rawPart)
				if part == nil {
					continue
				}
				if err := budget.addReference(len(source)); err != nil {
					return nil, err
				}
				content, err := parseInlineImageSource(source)
				if err != nil {
					return nil, err
				}
				if !bps && len(content.encoded) > base64.StdEncoding.EncodedLen(min(byteLimit, maxInlineImageTotal-budget.total)) {
					return nil, imageLimit("encoded_image_bytes", base64.StdEncoding.EncodedLen(min(byteLimit, maxInlineImageTotal-budget.total)), len(content.encoded))
				}
				newContent := false
				encodedIdentity := encodedKey{content.mimeType, inlineEncodedDigest(content.encoded)}
				if cached := encodings[encodedIdentity]; cached != nil {
					content = cached
				} else {
					if err := inspectInlineImage(ctx, content, byteLimit); err != nil {
						return nil, err
					}
					identity := contentKey{content.mimeType, content.digest}
					if cached := unique[identity]; cached != nil {
						content = cached
					} else {
						unique[identity] = content
						newContent = true
					}
					encodings[encodedIdentity] = content
				}
				if err := budget.addDecoded(content.bytes, newContent); err != nil {
					return nil, err
				}
				delete(part, "image_url")
				rewrite.images = append(rewrite.images, inlineImage{partIndex: index, part: part, content: content})
			}
			if len(rewrite.images) > 0 {
				rewrites = append(rewrites, rewrite)
			}
		}
	}
	return rewrites, nil
}

func inlineImageSource(raw json.RawMessage) (map[string]json.RawMessage, string) {
	var part map[string]json.RawMessage
	if json.Unmarshal(raw, &part) != nil {
		return nil, ""
	}
	var kind, source string
	if json.Unmarshal(part["type"], &kind) != nil || kind != "input_image" ||
		json.Unmarshal(part["image_url"], &source) != nil ||
		len(source) < len("data:") || !strings.EqualFold(source[:len("data:")], "data:") {
		return nil, ""
	}
	if existing := bytes.TrimSpace(part["file_id"]); len(existing) > 0 &&
		!bytes.Equal(existing, []byte("null")) && !bytes.Equal(existing, []byte(`""`)) {
		return nil, ""
	}
	return part, source
}

func parseInlineImageSource(source string) (*inlineImageContent, error) {
	if len(source) < len("data:") || !strings.EqualFold(source[:len("data:")], "data:") {
		return nil, ErrInvalidInlineImage
	}
	media, encoded, found := strings.Cut(source[len("data:"):], ",")
	mimeType, base64Data := strings.CutSuffix(strings.ToLower(media), ";base64")
	if !found || !base64Data || encoded == "" {
		return nil, ErrInvalidInlineImage
	}
	var extension string
	switch mimeType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/gif":
		extension = ".gif"
	case "image/webp":
		extension = ".webp"
	default:
		return nil, ErrInvalidInlineImage
	}
	return &inlineImageContent{encoded: encoded, mimeType: mimeType, extension: extension}, nil
}

func inlineEncodedDigest(source string) [sha256.Size]byte {
	hash := sha256.New()
	var buffer [32 << 10]byte
	for len(source) > 0 {
		n := copy(buffer[:], source)
		_, _ = hash.Write(buffer[:n])
		source = source[n:]
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

type inlineImageReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r inlineImageReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Validate and fingerprint one image without retaining its decoded bytes. A
// decoder cannot read past limit+1, even for malformed or padded large inputs.
func inspectInlineImage(ctx context.Context, image *inlineImageContent, limit int) error {
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(image.encoded))
	reader := io.LimitReader(decoder, int64(limit)+1)
	hash := sha256.New()
	var prefix [512]byte
	prefixSize, total := 0, 0
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer[:])
		if n > 0 {
			total += n
			if total > limit {
				return imageLimit("image_bytes", limit, total)
			}
			prefixSize += copy(prefix[prefixSize:], buffer[:n])
			_, _ = hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrInvalidInlineImage
		}
	}
	if http.DetectContentType(prefix[:prefixSize]) != image.mimeType {
		return ErrInvalidInlineImage
	}
	image.bytes = total
	copy(image.digest[:], hash.Sum(nil))
	return nil
}

func (c *Client) uploadImage(ctx context.Context, endpoint, token string, image inlineImage, bps bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var body bytes.Buffer
	// One pre-sized multipart buffer owns the decoded image only for this
	// serial cache miss. Preflight and other image descriptors retain no bytes.
	body.Grow(image.content.bytes + 2048)
	form := multipart.NewWriter(&body)
	fields := [][2]string{
		{"purpose", "vision"},
		{"expires_after[anchor]", "created_at"},
		{"expires_after[seconds]", "3600"},
	}
	if bps {
		// Original BPS attachment requests carry purpose=vision but do not
		// accept the public Files API expiration fields.
		fields = fields[:1]
	}
	for _, field := range fields {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return "", ErrImageUpload
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name": "file", "filename": "image" + image.content.extension,
	}))
	header.Set("Content-Type", image.content.mimeType)
	part, err := form.CreatePart(header)
	if err != nil {
		return "", ErrImageUpload
	}
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(image.content.encoded))
	if copied, err := io.Copy(part, inlineImageReader{ctx, io.LimitReader(decoder, int64(image.content.bytes)+1)}); err != nil || copied != int64(image.content.bytes) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrImageUpload
	}
	if err := form.Close(); err != nil {
		return "", ErrImageUpload
	}
	request, err := newRequest(ctx, http.MethodPost, endpoint, token, &body)
	if err != nil {
		return "", err
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrImageUpload
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return "", ErrImageUpload
	}
	data, err := ReadJSON(io.LimitReader(response.Body, maxUploadReplyBytes+1))
	if err != nil || len(data) > maxUploadReplyBytes {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrImageUpload
	}
	var file struct {
		ID    string `json:"id"`
		BPSID string `json:"openai_file_id"`
	}
	if json.Unmarshal(data, &file) != nil {
		return "", ErrImageUpload
	}
	if bps {
		file.ID = file.BPSID
	}
	if !validImageFileID(file.ID) {
		return "", ErrImageUpload
	}
	return file.ID, nil
}

func validImageFileID(id string) bool {
	if len(id) <= len("file-") || len(id) > 128 ||
		(!strings.HasPrefix(id, "file-") && !strings.HasPrefix(id, "file_")) {
		return false
	}
	return !strings.ContainsFunc(id[len("file-"):], func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
}
