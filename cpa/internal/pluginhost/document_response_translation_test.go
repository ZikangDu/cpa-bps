package pluginhost

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestDocumentPluginBareResponsesConvertsToNonEmptyChatReply(t *testing.T) {
	a := &executorAdapter{host: New()}
	prepared := preparedExecutorCall{req: coreexecutor.Request{Model: "fixture-documents", Payload: []byte(`{"input":"Parsed workbook data"}`)}, requestedFormat: sdktranslator.FormatOpenAI, outputFormat: sdktranslator.FormatCodex}
	raw := []byte(`{"id":"resp_doc_fixture","object":"response","status":"completed","model":"fixture-upstream-documents","output":[{"id":"msg_doc_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"DOCUMENT_ACCEPTED","annotations":[]}]}],"usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}}`)
	var state any
	got := a.translateExecutorResponse(context.Background(), prepared, raw, false, &state)
	if ojson.GetBytes(got, "choices.0.message.content").String() != "DOCUMENT_ACCEPTED" || ojson.GetBytes(got, "usage.total_tokens").Int() != 23 {
		t.Fatalf("plugin document reply empty or corrupted: %s", got)
	}
}
