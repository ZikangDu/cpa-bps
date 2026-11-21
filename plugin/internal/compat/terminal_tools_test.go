package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestClientToolDeliveryRequiresExplicitCompletedStatus(t *testing.T) {
	catalog, err := DeclaredTools(object(t, `{"tools":[{"type":"function","name":"lookup"},{"type":"custom","name":"patch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{
		`{"type":"function_call","name":"lookup","arguments":"{}","call_id":"call_fixture"}`,
		`{"type":"custom_tool_call","name":"patch","input":"fixture","call_id":"call_fixture"}`,
	} {
		for _, status := range []string{`"incomplete"`, `"failed"`, `"in_progress"`, `null`, `123`, `""`} {
			raw := json.RawMessage(fmt.Sprintf(`{"status":%s,"output":[%s]}`, status, call))
			if err := catalog.ValidateResponse(raw); !errors.Is(err, ErrInvalidToolResponse) {
				t.Fatalf("non-completed tool was exposed: status=%s err=%v", status, err)
			}
		}
		for _, prefix := range []string{`"status":"completed",`, ``} {
			if err := catalog.ValidateResponse(json.RawMessage(`{` + prefix + `"output":[` + call + `]}`)); err != nil {
				t.Fatalf("completed or legacy-status fixture rejected: %v", err)
			}
		}
	}
	if err := catalog.ValidateResponse(json.RawMessage(`{"status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial text"}]}]}`)); err != nil {
		t.Fatalf("non-tool partial response rejected: %v", err)
	}
}
