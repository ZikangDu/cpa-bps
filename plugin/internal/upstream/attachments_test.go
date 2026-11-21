package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRewriteInlineImagesUploadsOnlyDataURLs(t *testing.T) {
	imageBytes := fixturePNG(t)
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/files" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("incorrect Files request method, path or authorization")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error("Files request was not multipart")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fields := make(map[string]string)
		hasImage := false
		for {
			part, readErr := reader.NextPart()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				t.Error("failed to read multipart section")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			value, readErr := io.ReadAll(part)
			if readErr != nil {
				t.Error("failed to read multipart data")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FormName() == "file" {
				hasImage = part.FileName() == "image.png" && part.Header.Get("Content-Type") == "image/png" && bytes.Equal(value, imageBytes)
			} else {
				fields[part.FormName()] = string(value)
			}
		}
		if !hasImage || fields["purpose"] != "vision" ||
			fields["expires_after[anchor]"] != "created_at" || fields["expires_after[seconds]"] != "3600" {
			t.Error("expected image bytes, content type and expiration in the Files request")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"file-fixture-%d","unused":"discard"}`, uploads.Add(1))
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()

	payload := fmt.Appendf(nil, `{"model":"fixture","input":[
		{"type":"message","content":[
			{"type":"input_image","image_url":%q,"detail":"high","extension":"preserve"},
			{"type":"input_image","image_url":"https://fixture.invalid/image.png"},
			{"type":"input_image","file_id":"file-existing"}
		]},
		{"type":"function_call_output","output":[{"type":"input_image","image_url":%q}]}
	]}`, source, source)
	rewritten, err := client.RewriteInlineImages(context.Background(), server.URL+"/files", "fixture", payload)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Model string
		Input []struct {
			Content []map[string]string
			Output  []map[string]string
		}
	}
	if err := json.Unmarshal(rewritten, &result); err != nil {
		t.Fatal(err)
	}
	first := result.Input[0].Content
	if result.Model != "fixture" || uploads.Load() != 1 ||
		first[0]["file_id"] != "file-fixture-1" || first[0]["image_url"] != "" ||
		first[0]["detail"] != "high" || first[0]["extension"] != "preserve" ||
		first[1]["image_url"] != "https://fixture.invalid/image.png" ||
		first[2]["file_id"] != "file-existing" || result.Input[1].Output[0]["file_id"] != "file-fixture-1" {
		t.Fatalf("incorrect attachment rewrite: %s", rewritten)
	}

	unchanged := []byte(`{ "input": "plain text" }`)
	got, err := client.RewriteInlineImages(context.Background(), server.URL+"/files", "fixture", unchanged)
	if err != nil || !bytes.Equal(got, unchanged) || uploads.Load() != 1 {
		t.Fatalf("plain text caused a change or upload: %v", err)
	}
}

func TestRewriteBPSImagesUsesNativeAttachmentContract(t *testing.T) {
	contents := fixturePNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/basispoints/api/attachments" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("incorrect BPS attachment target")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["purpose"]) != 1 || r.MultipartForm.Value["purpose"][0] != "vision" || len(r.MultipartForm.File["file"]) != 1 {
			t.Error("BPS attachments must contain purpose=vision and one file, without expiration fields")
		}
		_, _ = io.WriteString(w, `{"openai_file_id":"file-bps-fixture"}`)
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	payload := fmt.Appendf(nil, `{"input":[{"role":"user","content":[{"type":"input_image","detail":"high","image_url":"data:image/png;base64,%s"}]}]}`,
		base64.StdEncoding.EncodeToString(contents))
	got, err := client.RewriteBPSImages(context.Background(), server.URL+"/basispoints/api/attachments", "fixture", payload)
	if err != nil || !bytes.Contains(got, []byte("file-bps-fixture")) || bytes.Contains(got, []byte("image_url")) || bytes.Contains(got, []byte("detail")) {
		t.Fatalf("native BPS attachment rewrite failed: %s %v", got, err)
	}
}

func TestRewriteInlineImagesPreflightsAllAttachments(t *testing.T) {
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG(t))
	validPart := fmt.Sprintf(`{"type":"input_image","image_url":%q}`, source)
	items := func(parts string) []byte {
		return fmt.Appendf(nil, `{"input":[{"type":"message","content":[%s]}]}`, parts)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("invalid input caused a Files request before all attachments were validated")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	for _, test := range []struct {
		name    string
		payload []byte
		wantErr error
	}{
		{"invalid next image", items(validPart + `,{"type":"input_image","image_url":"data:image/png;base64,not-base64"}`), ErrInvalidInlineImage},
		{"declared media differs from content", items(strings.ReplaceAll(validPart, "image/png", "image/gif")), ErrInvalidInlineImage},
		{"attachment count", items(strings.TrimSuffix(strings.Repeat(validPart+",", maxInlineImages+1), ",")), ErrInlineImageLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.RewriteInlineImages(context.Background(), server.URL, "fixture", test.payload)
			if !errors.Is(err, test.wantErr) {
				t.Errorf("got %v, wanted %v", err, test.wantErr)
			}
		})
	}
}

func TestDecodeInlineImageEnforcesRemainingBudget(t *testing.T) {
	imageBytes := fixturePNG(t)
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	if _, err := decodeInlineImage(source, len(imageBytes)-1); !errors.Is(err, ErrInlineImageLimit) {
		t.Fatalf("remaining byte budget error = %v", err)
	}
	oversize := "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(maxInlineImageBytes)+4)
	if _, err := decodeInlineImage(oversize, maxInlineImageTotal); !errors.Is(err, ErrInlineImageLimit) {
		t.Fatalf("single image size error = %v", err)
	}
}

func TestRewriteInlineImagesRejectsUploadRedirectAndInvalidID(t *testing.T) {
	var redirected atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/files-redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/redirected", func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/files-malformed", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"file-invalid/path"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	input := fmt.Appendf(nil, `{"input":[{"content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`,
		base64.StdEncoding.EncodeToString(fixturePNG(t)))
	for _, path := range []string{"/files-redirect", "/files-malformed"} {
		got, err := client.RewriteInlineImages(context.Background(), server.URL+path, "fixture", input)
		if got != nil || !errors.Is(err, ErrImageUpload) || redirected.Load() != 0 {
			t.Fatalf("%s returned %v with %d redirected requests", path, err, redirected.Load())
		}
	}
}

func fixturePNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
