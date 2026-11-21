package chat_completions

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestDocumentPluginBareResponsesNonStreamRetainsChatContentToolsAndUsage(t *testing.T) {
	raw := []byte(`{"id":"resp_doc_fixture","object":"response","status":"completed","model":"fixture-upstream-documents","created_at":1234,"output":[{"id":"msg_doc_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"DOCUMENT_ACCEPTED","annotations":[]}]},{"type":"function_call","name":"lookup_sheet","call_id":"call_sheet","arguments":"{\"sheet\":\"Sheet1\"}"}],"usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23,"input_tokens_details":{"cached_tokens":10}}}`)
	output := ConvertCodexResponseToOpenAINonStream(context.Background(), "fixture", nil, nil, raw, nil)
	for path, want := range map[string]string{"object": "chat.completion", "id": "resp_doc_fixture", "model": "fixture-upstream-documents", "choices.0.message.content": "DOCUMENT_ACCEPTED", "choices.0.message.tool_calls.0.function.name": "lookup_sheet", "choices.0.message.tool_calls.0.id": "call_sheet", "choices.0.message.tool_calls.0.function.arguments": "{\"sheet\":\"Sheet1\"}", "choices.0.finish_reason": "tool_calls"} {
		if got := ojson.GetBytes(output, path).String(); got != want {
			t.Fatalf("%s=%q want %q; %s", path, got, want, output)
		}
	}
	if ojson.GetBytes(output, "usage.prompt_tokens").Int() != 20 || ojson.GetBytes(output, "usage.completion_tokens").Int() != 3 || ojson.GetBytes(output, "usage.total_tokens").Int() != 23 || ojson.GetBytes(output, "usage.prompt_tokens_details.cached_tokens").Int() != 10 {
		t.Fatal("usage lost", string(output))
	}
}
func TestDocumentPluginBareIncompleteMatchesExistingTerminalEnvelope(t *testing.T) {
	bare := []byte(`{"id":"resp_partial","object":"response","created_at":1234,"status":"incomplete","model":"fixture","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}`)
	envelope := append([]byte(`{"type":"response.incomplete","response":`), bare...)
	envelope = append(envelope, '}')
	first := ConvertCodexResponseToOpenAINonStream(context.Background(), "fixture", nil, nil, bare, nil)
	second := ConvertCodexResponseToOpenAINonStream(context.Background(), "fixture", nil, nil, envelope, nil)
	if string(first) != string(second) || ojson.GetBytes(first, "choices.0.finish_reason").String() != "length" {
		t.Fatal("bare response changed terminal semantics")
	}
	for _, bad := range []string{`{"object":"response","status":"in_progress","output":[]}`, `{"object":"chat.completion","status":"completed","output":[]}`, `{"object":"response","status":"completed","output":"invalid"}`} {
		if len(ConvertCodexResponseToOpenAINonStream(context.Background(), "fixture", nil, nil, []byte(bad), nil)) != 0 {
			t.Fatal("invalid/nonterminal body accepted")
		}
	}
}

func TestDocumentPluginCompleteSSEAndBareJSONChatFrames(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_doc","model":"fixture","created_at":1234}}`
	delta := `{"type":"response.output_text.delta","delta":"DOCUMENT_ACCEPTED"}`
	final := `{"type":"response.completed","response":{"id":"resp_doc","model":"fixture","status":"completed","usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}}}`
	variants := [][][]byte{
		{[]byte(created), []byte(delta), []byte(final)},
		{[]byte("data: " + created), []byte("data: " + delta), []byte("data: " + final)},
		{[]byte("event: response.created\ndata: " + created + "\n\n"), []byte("event: response.output_text.delta\ndata: " + delta + "\n\n"), []byte("event: response.completed\ndata: " + final + "\n\n")},
		{[]byte("event: response.created\r\ndata: " + created + "\r\n\r\nevent: response.output_text.delta\r\ndata: " + delta + "\r\n\r\nevent: response.completed\r\ndata: " + final + "\r\n\r\ndata: [DONE]\r\n\r\n")},
	}
	for i, frames := range variants {
		var state any
		var output [][]byte
		for _, frame := range frames {
			output = append(output, ConvertCodexResponseToOpenAI(context.Background(), "fixture", nil, nil, frame, &state)...)
		}
		text, terminals := "", 0
		for _, frame := range output {
			text += ojson.GetBytes(frame, "choices.0.delta.content").String()
			if ojson.GetBytes(frame, "choices.0.finish_reason").String() == "stop" {
				terminals++
				if ojson.GetBytes(frame, "usage.total_tokens").Int() != 23 {
					t.Fatal("terminal usage lost")
				}
			}
		}
		if text != "DOCUMENT_ACCEPTED" || terminals != 1 {
			t.Fatalf("variant %d discarded or duplicated content: text=%q terminals=%d", i, text, terminals)
		}
	}
}
