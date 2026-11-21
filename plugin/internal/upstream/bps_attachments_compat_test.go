package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestBPSImageReferencesCanonicalizeExistingAndUploadedFiles(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-bps-reference" {
			t.Error("unexpected image request")
		}
		uploads.Add(1)
		_, _ = io.WriteString(w, `{"openai_file_id":"file-native-reference"}`)
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG(t))
	raw := fmt.Appendf(nil, `{"metadata":{"keep":"value"},"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-existing","detail":"high","extra":"remove-from-native-reference"},{"type":"input_image","image_url":%q,"detail":"low","extra":"remove-from-native-reference"},{"type":"input_image","image_url":"https://example.invalid/image.png","detail":"auto","extra":"keep-url-extension"}]},{"type":"function_call_output","call_id":"call-fixture","output":[{"type":"input_image","file_id":"file-output","detail":"high"}]},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":" opaque\\nvalue "},{"type":"input_image","file_id":"file-agent","detail":"low"}]}]}`, source)
	before := bytes.Clone(raw)
	got, err := client.RewriteBPSImages(context.Background(), server.URL, "fixture-bps-reference", raw)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Content []map[string]any
			Output  []map[string]any
		}
	}
	if json.Unmarshal(got, &body) != nil {
		t.Fatal("invalid rewritten body")
	}
	for i, id := range []string{"file-existing", "file-native-reference"} {
		if !reflect.DeepEqual(body.Input[0].Content[i], map[string]any{"type": "input_image", "file_id": id}) {
			t.Fatalf("noncanonical file ref: %v", body.Input[0].Content[i])
		}
	}
	if body.Input[0].Content[2]["extra"] != "keep-url-extension" || body.Input[0].Content[2]["image_url"] != "https://example.invalid/image.png" {
		t.Fatal("remote URL was modified or downloaded")
	}
	if len(body.Input[1].Output[0]) != 2 || body.Input[1].Output[0]["file_id"] != "file-output" {
		t.Fatal("tool image file reference was not canonicalized")
	}
	if body.Input[2].Content[0]["encrypted_content"] != " opaque\\nvalue " || len(body.Input[2].Content[1]) != 2 {
		t.Fatal("agent opaque state was changed")
	}
	if uploads.Load() != 1 || !bytes.Equal(raw, before) {
		t.Fatal("rewriting uploaded more than once or mutated caller input")
	}
}

func TestBPSImageConflictAndMalformedReferencesFailBeforeAnyUpload(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixturePNG(t))
	for _, part := range []string{
		`{"type":"input_image","file_id":"file-private","image_url":"https://private.invalid/file?secret=token"}`,
		fmt.Sprintf(`{"type":"input_image","file_id":"file-private","image_url":%q}`, source),
		`{"type":"input_image","file_id":37}`,
		`{"type":"input_image","image_url":{"url":"https://example.invalid/image.png"}}`,
		`{"type":"input_image","file_id":" "}`,
		`{"type":"input_image"}`,
	} {
		raw := fmt.Appendf(nil, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]},{"type":"custom_tool_call_output","output":[%s]}]}`, source, part)
		got, err := client.RewriteBPSImages(context.Background(), server.URL, "fixture", raw)
		if got != nil || !errors.Is(err, ErrInvalidInlineImage) || uploads.Load() != 0 {
			t.Fatalf("invalid image reached upload or accepted: %v", err)
		}
	}
}

func TestGenericFileReferenceExtensionsAreNotStripped(t *testing.T) {
	client := New()
	defer client.CloseIdleConnections()
	raw := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-existing","detail":"high","extra":"keep"}]}]}`)
	got, err := client.RewriteInlineImages(context.Background(), "https://example.invalid/files", "fixture", raw)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("BPS normalization leaked into generic Files API: %s %v", got, err)
	}
}

func TestBPSRejectsDocumentFileContentBeforeUpload(t *testing.T) {
	for _, part := range []string{`{"type":"input_file","file_id":"file-fixture"}`, `{"type":"input_file","filename":"fixture.xlsx","file_data":"data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64,AA=="}`} {
		raw := []byte(`{"input":[{"type":"message","role":"user","content":[` + part + `]}]}`)
		c := NewWithTransport(accountTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("unsupported document reached network")
			return nil, nil
		}))
		if _, err := c.RewriteBPSImages(context.Background(), "https://bps.openai.com/basispoints/api/attachments", "fixture-token", raw); err != ErrUnsupportedBPSFile {
			t.Fatalf("unexpected document error: %v", err)
		}
	}
}
