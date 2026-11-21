package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

const (
	maxInlineImages     = 8
	maxInlineImageBytes = 5 << 20
	maxInlineImageTotal = 16 << 20
	maxUploadReplyBytes = 64 << 10
)

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
	mimeType  string
	extension string
	data      []byte
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
	rewrites, err := inspectInlineImages(input)
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
	for _, rewrite := range rewrites {
		for _, image := range rewrite.images {
			key := attachmentCacheKey(filesURL, token, image, bps)
			fileID, errUpload := uploadedImages.get(ctx, key, func() (string, error) {
				return c.uploadImage(ctx, filesURL, token, image, bps)
			})
			if errUpload != nil {
				return nil, errUpload
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

func inspectInlineImages(input []json.RawMessage) ([]attachmentRewrite, error) {
	var rewrites []attachmentRewrite
	var count, total int
	for itemIndex, rawItem := range input {
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
				if count == maxInlineImages {
					return nil, ErrInlineImageLimit
				}
				image, err := decodeInlineImage(source, maxInlineImageTotal-total)
				if err != nil {
					return nil, err
				}
				image.partIndex, image.part = index, part
				rewrite.images = append(rewrite.images, image)
				total += len(image.data)
				count++
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

func decodeInlineImage(source string, remaining int) (inlineImage, error) {
	media, encoded, found := strings.Cut(source[len("data:"):], ",")
	mimeType, base64Data := strings.CutSuffix(strings.ToLower(media), ";base64")
	if !found || !base64Data || encoded == "" {
		return inlineImage{}, ErrInvalidInlineImage
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
		return inlineImage{}, ErrInvalidInlineImage
	}
	limit := min(maxInlineImageBytes, remaining)
	if len(encoded) > base64.StdEncoding.EncodedLen(limit) {
		return inlineImage{}, ErrInlineImageLimit
	}
	image, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || http.DetectContentType(image) != mimeType {
		return inlineImage{}, ErrInvalidInlineImage
	}
	if len(image) > limit {
		return inlineImage{}, ErrInlineImageLimit
	}
	return inlineImage{data: image, mimeType: mimeType, extension: extension}, nil
}

func (c *Client) uploadImage(ctx context.Context, endpoint, token string, image inlineImage, bps bool) (string, error) {
	var body bytes.Buffer
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
		"name": "file", "filename": "image" + image.extension,
	}))
	header.Set("Content-Type", image.mimeType)
	part, err := form.CreatePart(header)
	if err != nil {
		return "", ErrImageUpload
	}
	if _, err := part.Write(image.data); err != nil {
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
