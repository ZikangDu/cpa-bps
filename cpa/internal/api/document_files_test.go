package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func documentTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("CPA_DOCUMENTS_DIR", filepath.Join(t.TempDir(), "docs"))
	s := newTestServerWithConfig(t, &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"client-A", "client-B"}}})
	t.Cleanup(func() {
		if s.documentFiles != nil {
			_ = s.documentFiles.Close()
		}
	})
	return s
}
func documentRequest(s *Server, method, path, key, contentType string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}
func documentMultipart(t *testing.T, filename string, data []byte, duplicate bool) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("purpose", "user_data")
	p, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write(data)
	if duplicate {
		p, _ = w.CreateFormFile("file", "extra.csv")
		_, _ = p.Write([]byte("extra"))
	}
	_ = w.Close()
	return w.FormDataContentType(), body.Bytes()
}
func TestDocumentFilesHTTPAuthenticatedRoundTrip(t *testing.T) {
	s := documentTestServer(t)
	kind, data := documentMultipart(t, "table.csv", []byte("a,b\n2,3\n"), false)
	if w := documentRequest(s, "POST", "/v1/files", "", kind, bytes.NewReader(data)); w.Code != 401 {
		t.Fatal("unauthenticated upload", w.Code)
	}
	upload := documentRequest(s, "POST", "/v1/files", "client-A", kind, bytes.NewReader(data))
	if upload.Code != 200 {
		t.Fatal(upload.Code, upload.Body.String())
	}
	var file struct {
		ID        string `json:"id"`
		Bytes     int    `json:"bytes"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if json.Unmarshal(upload.Body.Bytes(), &file) != nil || file.ID == "" || file.Bytes != 8 || file.ExpiresAt == 0 {
		t.Fatal(upload.Body.String())
	}
	path := "/v1/files/" + file.ID
	for _, suffix := range []string{"", "/content"} {
		if w := documentRequest(s, "GET", path+suffix, "client-B", "", nil); w.Code != 404 {
			t.Fatal("cross caller read", w.Code)
		}
	}
	if w := documentRequest(s, "DELETE", path, "client-B", "", nil); w.Code != 404 {
		t.Fatal("cross caller delete", w.Code)
	}
	own := documentRequest(s, "GET", path+"/content", "client-A", "", nil)
	if own.Code != 200 || own.Body.String() != "a,b\n2,3\n" || own.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("content response changed")
	}
	list := documentRequest(s, "GET", "/v1/files", "client-B", "", nil)
	if strings.Contains(list.Body.String(), file.ID) {
		t.Fatal("cross caller list")
	}
	deleted := documentRequest(s, "DELETE", path, "client-A", "", nil)
	if deleted.Code != 200 || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatal(deleted.Body.String())
	}
	if w := documentRequest(s, "GET", path, "client-A", "", nil); w.Code != 404 {
		t.Fatal("deleted file visible")
	}
}
func TestDocumentFilesRejectUnsafeUploadsAndDisabledStore(t *testing.T) {
	s := documentTestServer(t)
	for _, tc := range []struct {
		name      string
		data      []byte
		duplicate bool
	}{{"../escape.csv", []byte("abc"), false}, {"fake.pdf", []byte("not pdf"), false}, {"one.csv", []byte("abc"), true}, {"empty.csv", nil, false}} {
		kind, data := documentMultipart(t, tc.name, tc.data, tc.duplicate)
		w := documentRequest(s, "POST", "/v1/files", "client-A", kind, bytes.NewReader(data))
		if w.Code != 400 {
			t.Fatalf("%s status=%d %s", tc.name, w.Code, w.Body.String())
		}
	}
	t.Setenv("CPA_DOCUMENTS_DIR", "")
	disabled := newTestServer(t)
	w := documentRequest(disabled, "GET", "/v1/files", "test-key", "", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("disabled Files API unexpectedly enabled", w.Code)
	}
}
