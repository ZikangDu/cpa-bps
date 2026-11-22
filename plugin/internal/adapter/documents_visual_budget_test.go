package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/documents"
)

func visualBudgetRuntime(t *testing.T) (*Runtime, config, executionRequest) {
	t.Helper()
	r := New(nil)
	t.Cleanup(r.Close)
	return r, config{OAuthPreset: "bps", DocumentWorkerPath: "synthetic-cached-worker"}, executionRequest{Metadata: map[string]any{"caller_scope": "visual-budget-caller"}}
}

// Populate the same immutable parser cache used by prepareDocuments with a
// synthetic PDF result. No external PDF process, network or model is involved.
func cachedVisualPDF(t *testing.T, r *Runtime, cfg config, req executionRequest, filename string, pages int, image []byte) json.RawMessage {
	t.Helper()
	data := []byte("%PDF-1.4\nSynthetic fixture " + filename)
	result := documents.Result{Format: "pdf", Text: "Visible text from " + filename, Pages: pages}
	for page := 1; page <= pages; page++ {
		result.Images = append(result.Images, documents.Image{Page: page, PNG: image})
	}
	_, err := r.documents.parseWithWorker(t.Context(), cfg.DocumentWorkerPath, cfg.DocumentPython, req.Metadata["caller_scope"].(string), documents.Job{Filename: filename, Data: data},
		func(context.Context, string, string, documents.Job) (documents.Result, error) { return result, nil })
	if err != nil {
		t.Fatal(err)
	}
	return inlineDocument(filename, string(data))
}

func visualImagePart(kind, value string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"type": "input_image", kind: value})
	return raw
}

func visualRequest(t *testing.T, r *Runtime, cfg config, req executionRequest, input []json.RawMessage) ([]json.RawMessage, []byte) {
	t.Helper()
	source, _ := json.Marshal(map[string]any{"model": "fixture", "input": input})
	before := bytes.Clone(source)
	prepared, err := r.prepareDocuments(cfg, req, source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, before) {
		t.Fatal("document preparation mutated caller history")
	}
	var body struct{ Input []json.RawMessage }
	if err := json.Unmarshal(prepared, &body); err != nil {
		t.Fatal(err)
	}
	return body.Input, prepared
}

func visualMessage(role string, parts ...json.RawMessage) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": role, "content": parts})
	return raw
}

func visualParts(t *testing.T, item json.RawMessage, field string) []map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	var parts []map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil || json.Unmarshal(fields[field], &parts) != nil {
		t.Fatal("invalid fixture output parts")
	}
	return parts
}

func visualCount(t *testing.T, item json.RawMessage, field string) int {
	t.Helper()
	count := 0
	for _, part := range visualParts(t, item, field) {
		if string(part["type"]) == `"input_image"` {
			count++
		}
	}
	return count
}

func TestOldImagesDoNotConsumeNewPDFVisualBudget(t *testing.T) {
	for _, kind := range []string{"inline screenshots", "file IDs and remote URLs"} {
		t.Run(kind, func(t *testing.T) {
			r, cfg, req := visualBudgetRuntime(t)
			var old []json.RawMessage
			for index := 0; index < 8; index++ {
				if kind == "inline screenshots" {
					old = append(old, visualImagePart("image_url", "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("old-image-%d", index)))))
				} else if index%2 == 0 {
					old = append(old, visualImagePart("file_id", fmt.Sprintf("file-existing-%d", index)))
				} else {
					old = append(old, visualImagePart("image_url", fmt.Sprintf("https://images.example.test/%d.png", index)))
				}
			}
			oldMessage := visualMessage("user", old...)
			doc := cachedVisualPDF(t, r, cfg, req, "new.pdf", 2, []byte("synthetic-rendered-page"))
			input, prepared := visualRequest(t, r, cfg, req, []json.RawMessage{oldMessage, visualMessage("user", doc)})
			if len(input) != 2 || !bytes.Equal(input[0], oldMessage) || visualCount(t, input[0], "content") != 8 || visualCount(t, input[1], "content") != 2 || bytes.Contains(prepared, []byte("was omitted")) {
				t.Fatal("old images were changed or suppressed new document pages")
			}
		})
	}
}

func TestNewDocumentVisualsShareEightPageBudgetAcrossDocuments(t *testing.T) {
	r, cfg, req := visualBudgetRuntime(t)
	var input []json.RawMessage
	for _, name := range []string{"first.pdf", "second.pdf", "third.pdf"} {
		input = append(input, visualMessage("user", cachedVisualPDF(t, r, cfg, req, name, 4, []byte("synthetic-page"))))
	}
	output, prepared := visualRequest(t, r, cfg, req, input)
	if len(output) != 3 || visualCount(t, output[0], "content") != 4 || visualCount(t, output[1], "content") != 4 || visualCount(t, output[2], "content") != 0 {
		t.Fatal("document-rendering count budget changed")
	}
	if strings.Count(string(prepared), "was omitted") != 4 || !strings.Contains(string(output[2]), "document-rendering image-count or image-byte limit") || !strings.Contains(string(output[2]), "Visible text from third.pdf") {
		t.Fatal("omitted document visuals were not identified without losing text")
	}
}

func TestNewDocumentVisualsShareSixteenMiBByteBudget(t *testing.T) {
	r, cfg, req := visualBudgetRuntime(t)
	image := bytes.Repeat([]byte{0x42}, 4<<20)
	first := cachedVisualPDF(t, r, cfg, req, "large-first.pdf", 3, image)
	second := cachedVisualPDF(t, r, cfg, req, "large-second.pdf", 3, image)
	output, prepared := visualRequest(t, r, cfg, req, []json.RawMessage{visualMessage("user", first), visualMessage("user", second)})
	if visualCount(t, output[0], "content") != 3 || visualCount(t, output[1], "content") != 1 || strings.Count(string(prepared), "was omitted") != 2 {
		t.Fatal("combined document visual bytes exceeded the sixteen MiB budget")
	}
	if !strings.Contains(string(output[1]), "Document visual page 2") || !strings.Contains(string(output[1]), "Document visual page 3") || !strings.Contains(string(output[1]), "large-second.pdf") {
		t.Fatal("byte-budget omission did not identify the excluded pages")
	}
}

func TestRepeatedDocumentVisualsKeepHistoryPositionsAndToolAssociation(t *testing.T) {
	r, cfg, req := visualBudgetRuntime(t)
	doc := cachedVisualPDF(t, r, cfg, req, "repeated.pdf", 2, []byte("same-page-content"))
	oldImage := visualImagePart("file_id", "file-existing-history")
	toolResult, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": "call_document", "output": []json.RawMessage{oldImage, doc}})
	input := []json.RawMessage{visualMessage("user", doc), json.RawMessage(`{"role":"assistant","content":"Seen first occurrence"}`), toolResult, json.RawMessage(`{"role":"user","content":"Continue"}`)}
	for range 2 {
		output, prepared := visualRequest(t, r, cfg, req, input)
		if len(output) != 5 || visualCount(t, output[0], "content") != 2 || visualCount(t, output[2], "output") != 1 || visualCount(t, output[3], "content") != 2 || !bytes.Equal(output[1], input[1]) || !bytes.Equal(output[4], input[3]) {
			t.Fatal("repeated document visuals moved or disappeared across turns")
		}
		var result map[string]json.RawMessage
		_ = json.Unmarshal(output[2], &result)
		if string(result["call_id"]) != `"call_document"` || !bytes.Equal(visualParts(t, output[2], "output")[0]["file_id"], json.RawMessage(`"file-existing-history"`)) || strings.Count(string(prepared), "Visible text from repeated.pdf") != 2 || bytes.Contains(prepared, []byte("was omitted")) {
			t.Fatal("parser-cache reuse dropped an occurrence or changed its original tool/image association")
		}
	}
}
