package adapter

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDocumentRecoveryKeepsOriginalConversationTurns(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	original := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"First file"},` + string(doc) + `]},{"role":"assistant","content":"Seen"},{"role":"user","content":[{"type":"text","text":"Same file again"},` + string(doc) + `]},{"role":"assistant","content":"Seen again"},{"role":"user","content":"Which turns had files?"}]}`)
	for _, first := range []string{`{"type":"input_text","text":"First file"}`, `{"type":"input_text","text":"First file"},` + string(doc)} {
		source := []byte(`{"input":[{"role":"user","content":[` + first + `]},{"role":"assistant","content":"Seen"},{"role":"user","content":"Same file again"},{"role":"assistant","content":"Seen again"},{"role":"user","content":"Which turns had files?"}]}`)
		req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}, OriginalRequest: original}
		got, err := r.prepareDocuments(cfg, req, source)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Input []json.RawMessage }
		_ = json.Unmarshal(got, &body)
		if len(body.Input) != 5 || !strings.Contains(string(body.Input[0]), "A2=120") || !strings.Contains(string(body.Input[2]), "A2=120") || strings.Contains(string(body.Input[4]), "Document data") {
			t.Fatalf("historical documents moved to the newest turn: %s", got)
		}
		if strings.Count(string(got), "A2=120") != 2 {
			t.Fatal("repeated document occurrence was dropped or duplicated")
		}
	}
}

func TestDocumentRecoveryKeepsToolResultAssociation(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	req := executionRequest{Metadata: map[string]any{"caller_scope": "scope"}, OriginalRequest: []byte(`{"messages":[{"role":"tool","tool_call_id":"call-sheet","content":[` + string(doc) + `]},{"role":"user","content":"Use the earlier result"}]}`)}
	source := []byte(`{"input":[{"type":"function_call_output","call_id":"call-sheet","output":""},{"role":"user","content":"Use the earlier result"}]}`)
	got, err := r.prepareDocuments(cfg, req, source)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Input []map[string]json.RawMessage }
	_ = json.Unmarshal(got, &body)
	if len(body.Input) != 2 || string(body.Input[0]["call_id"]) != `"call-sheet"` || !strings.Contains(string(body.Input[0]["output"]), "A2=120") || strings.Contains(string(body.Input[1]["content"]), "Document data") {
		t.Fatalf("tool document lost its original call association: %s", got)
	}
}

func TestDocumentRecoveryRejectsMissingHistoryPosition(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	req := executionRequest{OriginalRequest: []byte(`{"messages":[{"role":"user","content":[` + string(doc) + `]},{"role":"user","content":"Second turn"}]}`)}
	_, err := r.prepareDocuments(cfg, req, []byte(`{"input":"Second turn"}`))
	if err == nil || err.Code() != "bps_document_history_unavailable" {
		t.Fatalf("missing turn was silently moved to the final turn: %v", err)
	}
}

func TestDocumentRecoveryLeavesAlreadyTranslatedHistoryAlone(t *testing.T) {
	r, cfg, _ := documentTestRuntime(t)
	doc := inlineDocument("sales.csv", "a,b\n1,2")
	// Host translation can omit empty assistant messages with only tool_calls.
	// An already retained document needs no positional recovery in that case.
	req := executionRequest{OriginalRequest: []byte(`{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"assistant","content":[` + string(doc) + `]}]}`)}
	got, err := r.prepareDocuments(cfg, req, []byte(`{"input":[{"role":"assistant","content":[`+string(doc)+`]}]}`))
	if err != nil || strings.Count(string(got), "A2=120") != 1 {
		t.Fatalf("already retained document was rejected or duplicated: %s %v", got, err)
	}
}

func TestDocumentPreparationHonorsParentCancellation(t *testing.T) {
	r, cfg, calls := documentTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := executionRequest{parentContext: ctx}
	_, err := r.prepareDocuments(cfg, req, documentBody(inlineDocument("sales.csv", "a,b\n1,2")))
	if err == nil || err.Code() != "bps_document_processing_canceled" {
		t.Fatalf("canceled parent reached document processing: %v", err)
	}
	if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
		t.Fatal("canceled request started a document worker")
	}
}
