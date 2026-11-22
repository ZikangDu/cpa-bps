package pluginhost

import (
	"context"
	"errors"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/codex"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The real BPS plugin advertises accepted client aliases and normalizes them on
// its wire boundary. Host validation must honor those capabilities without
// relaxing unsupported levels for any other model or provider.
func TestBPSDeclaredEffortAliasesReachPluginAcrossProtocols(t *testing.T) {
	const provider, model, client = "cpa-bps", "gpt-5.6-sol", "bps-effort-alias-regression"
	levels := []string{"none", "low", "medium", "high", "xhigh"}
	info := &registry.ModelInfo{ID: model, OwnedBy: provider, Thinking: &registry.ThinkingSupport{Levels: levels}}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(client, provider, []*registry.ModelInfo{info})
	t.Cleanup(func() { reg.UnregisterClient(client) })
	record := normalizeTestCapabilityRecord(capabilityRecord{id: "bps-effort-alias-plugin"})
	host := newHostWithRecords(record)
	host.modelRegistrations[record.id] = pluginModelRegistration{pluginID: record.id, provider: provider, models: []*registry.ModelInfo{info}, hasExecutor: true}
	var received []pluginapi.ExecutorRequest
	adapter := newExecutorAdapterForRecordForTest(host, record, &fakeExecutor{
		identifier: provider,
		execute: func(_ context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
			received = append(received, request)
			return pluginapi.ExecutorResponse{Payload: []byte(`{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","id":"msg_fixture","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)}, nil
		},
		executeStream: func(_ context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
			received = append(received, request)
			chunks := make(chan pluginapi.ExecutorStreamChunk)
			close(chunks)
			return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
		},
		countTokens: func(_ context.Context, request pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
			received = append(received, request)
			return pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":1}`)}, nil
		},
	}, []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex}, []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex})
	adapter.provider = provider

	// This is the user's original failure before the plugin advertised its alias.
	_, errOriginal := adapter.Execute(context.Background(), nil, coreexecutor.Request{Model: model, Format: sdktranslator.FormatOpenAIResponse, Payload: []byte(`{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"max"}}`)}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse})
	var thinkingErr *thinking.ThinkingError
	if !errors.As(errOriginal, &thinkingErr) || thinkingErr.Code != thinking.ErrLevelNotSupported || len(received) != 0 {
		t.Fatalf("unadvertised max: error=%v plugin calls=%d", errOriginal, len(received))
	}
	info.Thinking.Levels = append(levels, "max", "ultra", "x-high")
	reg.RegisterClient(client, provider, []*registry.ModelInfo{info})

	for _, tc := range []struct {
		name, suffix, body, want string
		format                   sdktranslator.Format
		override, filter         bool
	}{
		{"responses max", "", `{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"max"}}`, "max", sdktranslator.FormatOpenAIResponse, false, false},
		{"codex max", "", `{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"max"}}`, "max", sdktranslator.FormatCodex, false, false},
		{"responses ultra", "", `{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"ultra"}}`, "ultra", sdktranslator.FormatOpenAIResponse, false, false},
		{"responses x-high", "", `{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"x-high"}}`, "x-high", sdktranslator.FormatOpenAIResponse, false, false},
		{"chat max", "", `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`, "max", sdktranslator.FormatOpenAI, false, false},
		{"claude max", "", `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "max", sdktranslator.FormatClaude, false, false},
		{"max suffix wins", "(max)", `{"model":"gpt-5.6-sol(max)","input":"hi","reasoning":{"effort":"low"}}`, "max", sdktranslator.FormatOpenAIResponse, false, false},
		{"ultra suffix wins", "(ultra)", `{"model":"gpt-5.6-sol(ultra)","input":"hi","reasoning":{"effort":"low"}}`, "ultra", sdktranslator.FormatOpenAIResponse, false, false},
		{"x-high suffix wins", "(x-high)", `{"model":"gpt-5.6-sol(x-high)","input":"hi","reasoning":{"effort":"low"}}`, "x-high", sdktranslator.FormatOpenAIResponse, false, false},
		{"configuration update", "", `{"model":"gpt-5.6-sol","input":[{"type":"configuration_update","reasoning":{"effort":"max"}},{"role":"user","content":"hi"}],"reasoning":{"effort":"low"}}`, "max", sdktranslator.FormatOpenAIResponse, false, false},
		{"payload override wins", "(max)", `{"model":"gpt-5.6-sol(max)","input":"hi"}`, "low", sdktranslator.FormatOpenAIResponse, true, false},
		{"payload filter wins", "(max)", `{"model":"gpt-5.6-sol(max)","input":"hi"}`, "", sdktranslator.FormatOpenAIResponse, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host.runtimeConfig = &config.Config{}
			if tc.override {
				host.runtimeConfig.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: tc.format.String()}}, Params: map[string]any{"reasoning.effort": "low"}}}
			}
			if tc.filter {
				host.runtimeConfig.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: tc.format.String()}}, Params: []string{"reasoning.effort"}}}
			}
			received = nil
			req := coreexecutor.Request{Model: model + tc.suffix, Format: tc.format, Payload: []byte(tc.body)}
			opts := coreexecutor.Options{SourceFormat: tc.format, ResponseFormat: tc.format}
			if _, err := adapter.Execute(context.Background(), nil, req, opts); err != nil {
				t.Fatal(err)
			}
			opts.Stream = true
			stream, err := adapter.ExecuteStream(context.Background(), nil, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			for range stream.Chunks {
			}
			opts.Stream = false
			if _, err := adapter.CountTokens(context.Background(), nil, req, opts); err != nil {
				t.Fatal(err)
			}
			if len(received) != 3 {
				t.Fatalf("plugin calls = %d, want 3", len(received))
			}
			for _, request := range received {
				if effort := ojson.GetBytesNoCopy(request.Payload, "reasoning.effort").String(); effort != tc.want || request.Model != model {
					t.Fatalf("plugin model/effort = %q/%q, want %q/%q", request.Model, effort, model, tc.want)
				}
			}
		})
	}
}
