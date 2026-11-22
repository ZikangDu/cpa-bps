package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cpa-bps-reconstruction/internal/documents"
)

func documentTestRuntime(t *testing.T) (*Runtime, config, string) {
	t.Helper()
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker")
	calls := filepath.Join(dir, "calls")
	reply, _ := json.Marshal(documents.Reply{Result: &documents.Result{Format: "csv", Text: "Sheet CSV\nA1=Revenue\nA2=120\nB1=Costs\nB2=80\nC2 formula=A2-B2 cached=40"}})
	script := "#!/bin/sh\ncat >/dev/null\nprintf x >> '" + calls + "'\nprintf '%s' '" + string(reply) + "'\n"
	if os.WriteFile(worker, []byte(script), 0700) != nil {
		t.Fatal("fixture worker creation failed")
	}
	r := New(nil)
	t.Cleanup(r.Close)
	cfg := config{OAuthPreset: "bps", DocumentWorkerPath: worker}
	return r, cfg, calls
}
func inlineDocument(filename, data string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "input_file", "filename": filename, "file_data": base64.StdEncoding.EncodeToString([]byte(data))})
	return raw
}
func documentBody(parts ...json.RawMessage) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "fixture", "input": []any{map[string]any{"role": "user", "content": parts}}})
	return raw
}

func TestAutomaticInlineDocumentNormalizationAndNoSilentAttachmentDrop(t *testing.T) {
	r, cfg, calls := documentTestRuntime(t)
	req := executionRequest{AuthID: "test", Metadata: map[string]any{"caller_scope": "caller-A"}}
	doc := inlineDocument("sales.csv", "Revenue,Costs\n120,80")
	for _, source := range [][]byte{documentBody(doc), []byte(`{"model":"fixture","input":"Read the attachment","attachments":[` + string(doc) + `]}`), []byte(`{"model":"fixture","input":"Read the file","files":[` + string(doc) + `]}`)} {
		req.OriginalRequest = source
		got, err := r.prepareDocuments(cfg, req, source)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "A2=120") || strings.Contains(string(got), "file_data") || strings.Contains(string(got), `"attachments"`) {
			t.Fatal("document was dropped or raw file leaked upstream")
		}
	}
	n, _ := os.ReadFile(calls)
	if len(n) != 1 {
		t.Fatalf("same caller immutable content not cached: %d", len(n))
	}
	req.Metadata["caller_scope"] = "caller-B"
	req.OriginalRequest = nil
	if _, err := r.prepareDocuments(cfg, req, documentBody(doc)); err != nil {
		t.Fatal(err)
	}
	n, _ = os.ReadFile(calls)
	if len(n) != 2 {
		t.Fatal("parser cache crossed callers")
	}
}
func TestDocumentFileIDUsesOriginalRequestScopeAndForeignIDsFail(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	req := executionRequest{HostCallbackID: "fixture-context", Metadata: map[string]any{"caller_scope": "scope"}}
	r.host = func(method string, payload any, out any) error {
		fields := payload.(map[string]string)
		if method != "host.documents.read" || fields["host_callback_id"] != "fixture-context" || fields["file_id"] != "file-cpa-doc-owned" || len(fields) != 2 {
			return errors.New("unauthorized fixture")
		}
		raw, _ := json.Marshal(map[string]any{"filename": "sales.csv", "data": []byte("a,b\n1,2")})
		return json.Unmarshal(raw, out)
	}
	got, err := r.prepareDocuments(cfg, req, documentBody(json.RawMessage(`{"type":"input_file","file_id":"file-cpa-doc-owned"}`)))
	if err != nil || !strings.Contains(string(got), "Revenue") {
		t.Fatalf("owned file not parsed: %v", err)
	}
	for _, id := range []string{"file-cpa-doc-other", "file-other-provider"} {
		raw, _ := json.Marshal(map[string]string{"type": "input_file", "file_id": id})
		_, err := r.prepareDocuments(cfg, req, documentBody(raw))
		if err == nil || err.StatusCode() != 404 {
			t.Fatal("unowned file accepted")
		}
	}
}
func TestDocumentRejectedReferencesAndGenericProviderCompatibility(t *testing.T) {
	r, cfg, calls := documentTestRuntime(t)
	req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}}
	for _, part := range []string{`{"type":"input_file","filename":"x.csv","file_data":"not base64"}`, `{"type":"input_file","file_url":"http://127.0.0.1/private"}`, `{"type":"input_file","file_id":"file-cpa-doc-test","file_data":"YQ=="}`} {
		if _, err := r.prepareDocuments(cfg, req, documentBody(json.RawMessage(part))); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("invalid reference started parser")
	}
	cfg.OAuthPreset = ""
	source := []byte(`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-provider-owned"}]}],"attachments":[{"file_id":"file-external"}]}`)
	got, err := r.prepareDocuments(cfg, req, source)
	if err != nil || string(got) != string(source) {
		t.Fatal("generic provider file references were changed")
	}
}
func TestDocumentRecoveryFromHostTranslatedOriginal(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}, OriginalRequest: []byte(`{"messages":[{"role":"user","content":[` + string(doc) + `]}]}`)}
	got, err := r.prepareDocuments(cfg, req, []byte(`{"model":"fixture","input":"Analyze this file"}`))
	if err != nil || !strings.Contains(string(got), "Revenue") {
		t.Fatal("host-stripped document not restored")
	}
}
func TestDocumentParserConcurrentDeduplication(t *testing.T) {
	r, cfg, calls := documentTestRuntime(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := r.documents.parse(context.Background(), cfg.DocumentWorkerPath, "", "same-caller", documents.Job{Filename: "sales.csv", Data: []byte("a,b\n1,2")}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	n, _ := os.ReadFile(calls)
	if len(n) != 1 {
		t.Fatalf("same document parsed %d times", len(n))
	}
}

func TestDocumentAttachmentsWithoutInputAndTranslatedDedup(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}}
	for _, source := range []string{`{"model":"fixture","attachments":[` + string(doc) + `]}`, `{"model":"fixture","input":null,"files":[` + string(doc) + `]}`} {
		got, err := r.prepareDocuments(cfg, req, []byte(source))
		if err != nil || !strings.Contains(string(got), "Revenue") {
			t.Fatal("attachment-only input dropped", err)
		}
	}
	if _, err := r.prepareDocuments(cfg, req, []byte(`{"model":"fixture","input":123,"files":[`+string(doc)+`]}`)); err == nil {
		t.Fatal("invalid input bypassed parsing")
	}
	a := json.RawMessage(`{"type":"input_file","file_id":"file-cpa-doc-same","filename":"x.pdf"}`)
	b := json.RawMessage(`{"type":"input_file","file_id":"file-cpa-doc-same"}`)
	if documentReferenceKey(a) != documentReferenceKey(b) {
		t.Fatal("file identity depends on presentation filename")
	}
	raw := inlineDocument("x.csv", "a,b\n1,2")
	var original map[string]any
	_ = json.Unmarshal(raw, &original)
	original["file_data"] = "data:text/csv;base64," + original["file_data"].(string)
	uri, _ := json.Marshal(original)
	if documentReferenceKey(raw) != documentReferenceKey(uri) {
		t.Fatal("translated inline data was duplicated")
	}
	req.OriginalRequest = []byte(`{"messages":[{"role":"user","content":[` + string(uri) + `]}]}`)
	got, err := r.prepareDocuments(cfg, req, documentBody(raw))
	if err != nil || strings.Count(string(got), "A2=120") != 1 {
		t.Fatal("translated document appended twice", err)
	}
}

func TestOriginalMalformedAttachmentCannotDisappearAndSingleObjectWorks(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"Analyze"}],"attachments":123}`)}
	if _, err := r.prepareDocuments(cfg, req, []byte(`{"model":"fixture","input":"Analyze"}`)); err == nil {
		t.Fatal("malformed original attachment silently disappeared")
	}
	req.OriginalRequest = nil
	got, err := r.prepareDocuments(cfg, req, []byte(`{"model":"fixture","attachments":`+string(doc)+`}`))
	if err != nil || !strings.Contains(string(got), "Revenue") {
		t.Fatal("single-object attachment not parsed", err)
	}
}

func TestTSVDocumentDataURIDoesNotRequireFilename(t *testing.T) {
	d := &documentRequest{}
	raw, _ := json.Marshal(map[string]string{"type": "input_file", "file_data": "data:text/tab-separated-values;base64," + base64.StdEncoding.EncodeToString([]byte("A\tB\n1\t2"))})
	if !d.supported(raw) {
		t.Fatal("TSV MIME reference was not recognized")
	}
	file, err := d.resolve(raw)
	if err != nil || file.Filename != "document.tsv" || string(file.Data) != "A\tB\n1\t2" {
		t.Fatalf("TSV MIME reference lost its format: %#v, %v", file, err)
	}
}

func FuzzInlineDocumentEncodingKeepsIdentityAndFormat(f *testing.F) {
	f.Add([]byte("A,B\n1,2"), "sales.csv")
	f.Add([]byte("A\tB\n1\t2"), "report.tsv")
	f.Add([]byte("%PDF-1.4\nfixture"), "document.pdf")
	f.Fuzz(func(t *testing.T, data []byte, filename string) {
		if len(data) == 0 || len(data) > 4096 || len(filename) > 512 || filename == "" {
			return
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		raw, _ := json.Marshal(map[string]string{"type": "input_file", "filename": filename, "file_data": encoded})
		uri, _ := json.Marshal(map[string]string{"type": "input_file", "filename": filename, "file_data": "data:application/octet-stream;base64," + encoded})
		if documentReferenceKey(raw) != documentReferenceKey(uri) {
			t.Fatal("equivalent inline encodings lost document identity")
		}
		d := &documentRequest{}
		first, err := d.resolve(raw)
		if err != nil {
			t.Fatal(err)
		}
		second, err := d.resolve(uri)
		if err != nil || first.Filename != second.Filename || string(first.Data) != string(second.Data) {
			t.Fatal("equivalent inline encodings changed parsing format or content")
		}
	})
}
