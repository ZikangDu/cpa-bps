package adapter

import (
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/bpsprotocol"
)

func TestProtocolErrorExposesOnlyCategorizedReason(t *testing.T) {
	_, bridge, err := bpsprotocol.PrepareWithOptions([]byte(`{"model":"fixture","input":"fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`), "scope", bpsprotocol.Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Transform([]byte(`{"status":"completed","output":[{"type":"function_call","name":"run_officejs","call_id":"c","id":"fc","arguments":"SECRET_INVALID_JSON"}]}`))
	if err == nil {
		t.Fatal("malformed relay accepted")
	}
	got := protocolError(err).Error()
	if !strings.Contains(got, "response_tool_conversion: outer_arguments_invalid") || strings.Contains(got, "SECRET") || strings.Contains(got, "byte_offset") {
		t.Fatalf("unsafe or missing diagnostic: %s", got)
	}
}
