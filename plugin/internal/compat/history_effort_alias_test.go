package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestConfigurationUpdateAliasesAreStrictFallbacks(t *testing.T) {
	for _, effort := range []string{"max", "ultra", " MAX ", " ULTRA "} {
		for _, explicit := range []string{"", `,"reasoning":{"effort":"low","summary":"auto"}`, `,"reasoning_effort":"high"`} {
			t.Run(effort+explicit, func(t *testing.T) {
				body := object(t, fmt.Sprintf(`{"input":[{"type":"configuration_update","reasoning":{"effort":%q,"mode":"standard"}},{"role":"user","content":"fixture"}]%s}`, effort, explicit))
				if err := PrepareHistory(body, false); err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(body["input"], []byte("configuration_update")) || !bytes.Contains(body["input"], []byte("fixture")) {
					t.Fatal("recognized update was forwarded or ordinary history was dropped")
				}
				switch explicit {
				case "":
					if string(body["reasoning"]) != `{"effort":"xhigh"}` {
						t.Fatalf("alias fallback = %s", body["reasoning"])
					}
				case `,"reasoning_effort":"high"`:
					if string(body["reasoning_effort"]) != `"high"` || len(body["reasoning"]) != 0 {
						t.Fatal("alias update overrode explicit top-level effort")
					}
				default:
					if string(body["reasoning"]) != `{"effort":"low","summary":"auto"}` {
						t.Fatal("alias update overrode explicit nested effort")
					}
				}
			})
		}
	}
	for _, update := range []string{
		`{"type":"configuration_update","reasoning":{"effort":"max"},"future_extension":{"retain":true}}`,
		`{"type":"configuration_update","reasoning":{"effort":"ultra","future_extension":true}}`,
		`{"type":"configuration_update","reasoning":{"effort":"max","mode":"future"}}`,
		`{"type":"configuration_update","reasoning":{"effort":"ultra","summary":"auto"}}`,
		`{"type":"configuration_update","reasoning":{"effort":77}}`,
	} {
		body := object(t, `{"input":[`+update+`]}`)
		before, _ := json.Marshal(body)
		if err := PrepareHistory(body, false); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(body)
		if !bytes.Equal(before, after) {
			t.Fatal("unrecognized configuration update lost fields")
		}
	}
}
