package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"sync"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/compat"
	"cpa-bps-reconstruction/internal/upstream"
)

const maxAutomaticAttempts = 3

// Tools remain withheld until the complete response validates. Optional native
// final-answer text can commit earlier; after that point no generation replay
// is permitted. Before commitment, the existing bounded fallback still applies.
func (r *Runtime) executeAutoBuffered(cfg config, req executionRequest, source []byte, tools compat.ToolCatalog, binding [32]byte, mode string, stream bool, stopRequest func()) (any, error) {
	parent := r.ctx
	if req.parentContext != nil {
		parent = req.parentContext
	}
	ctx, cancel := context.WithTimeout(parent, executionWindow)
	req.parentContext = ctx
	cleanup := sync.OnceFunc(func() { cancel(); stopRequest(); r.tasks.Done() })
	transferred := false
	defer func() {
		if !transferred {
			cleanup()
		}
	}()
	payload, bridge, err := prepareAutomaticPayload(cfg, req, source, mode)
	if err != nil {
		return nil, protocolError(err)
	}
	cfg.BPSToolTransport = mode
	response, readCtx, finish, err := r.open(cfg, req, payload, stream)
	if err != nil {
		return nil, err
	}
	first := autoOpenedResponse{response: response, ctx: readCtx, finish: finish}
	var final []byte
	if stream && bridge.CanStreamNativeFinalText() {
		incremental := newAutoTextStream(tools, func(frame []byte) error {
			return r.host("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": frame}, nil)
		})
		type completed struct {
			body []byte
			err  error
		}
		done := make(chan completed, 1)
		go func() {
			body, runErr := r.runAutomaticAttempts(cfg, req, source, tools, binding, mode, stream, payload, bridge, first, incremental)
			if !incremental.activated {
				done <- completed{body, runErr}
				return
			}
			defer cleanup()
			if runErr == nil {
				runErr = incremental.complete()
			}
			r.closeAutomaticStream(req.StreamID, runErr)
		}()
		select {
		case <-incremental.ready:
			// The reader cannot emit into the host stream until its RPC is
			// about to return, so a long prefix cannot deadlock the bridge.
			transferred = true
			close(incremental.resume)
			return map[string]any{"headers": http.Header{"Content-Type": {eventStream}}}, nil
		case result := <-done:
			final, err = result.body, result.err
		}
	} else {
		final, err = r.runAutomaticAttempts(cfg, req, source, tools, binding, mode, stream, payload, bridge, first, nil)
	}
	if err != nil {
		return nil, err
	}
	if !stream {
		return map[string]any{"Payload": final, "Headers": http.Header{"Content-Type": {"application/json"}}}, nil
	}
	frames, err := bpsprotocol.ResponseStream(final)
	if err != nil {
		return nil, protocolError(err)
	}
	transferred = true
	go func() {
		defer cleanup()
		for _, frame := range bytes.Split(frames, []byte("\n\n")) {
			if len(frame) == 0 || bytes.Equal(frame, []byte("data: [DONE]")) {
				continue
			}
			if r.host("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": append(frame, '\n', '\n')}, nil) != nil {
				return
			}
		}
		_ = r.host("host.stream.close", map[string]any{"stream_id": req.StreamID}, nil)
	}()
	return map[string]any{"headers": http.Header{"Content-Type": {eventStream}}}, nil
}

type autoOpenedResponse struct {
	response *http.Response
	ctx      context.Context
	finish   func()
}

func prepareAutomaticPayload(cfg config, req executionRequest, source []byte, mode string) ([]byte, *bpsprotocol.Bridge, error) {
	return bpsprotocol.PrepareWithOptions(source, bpsCallerScope(req), bpsprotocol.Options{ToolTransport: mode, ToolsVersionID: cfg.BPSToolsVersionID})
}

func (r *Runtime) runAutomaticAttempts(cfg config, req executionRequest, source []byte, tools compat.ToolCatalog, binding [32]byte, mode string, stream bool, payload []byte, bridge *bpsprotocol.Bridge, first autoOpenedResponse, incremental *autoTextStream) ([]byte, error) {
	prepare := func(mode string) ([]byte, *bpsprotocol.Bridge, error) {
		return prepareAutomaticPayload(cfg, req, source, mode)
	}
	var earlier [][]byte
	switched := false
	repairs := 0
	for attempt := 0; attempt < maxAutomaticAttempts; attempt++ {
		cfg.BPSToolTransport = mode
		response, readCtx, finish := first.response, first.ctx, first.finish
		if attempt > 0 {
			var openErr error
			response, readCtx, finish, openErr = r.open(cfg, req, payload, stream)
			if openErr != nil {
				return nil, openErr
			}
		}
		var emit func([]byte) error
		if incremental != nil {
			incremental.beginAttempt(bridge.CanStreamNativeFinalText())
			emit = incremental.accept
		}
		var err error
		kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		var body []byte
		if kind == eventStream {
			body, err = upstream.ReadEvents(readCtx, response.Body, emit)
		} else {
			body, err = upstream.ReadResponseJSON(response.Body)
		}
		if err != nil {
			problem := resultError(readCtx, err, "bps_stream_interrupted")
			releaseGeneration := sync.OnceFunc(func() { response.Body.Close(); finish() })
			r.observeStreamQuotaRejection(readCtx, cfg, req, err, releaseGeneration)
			releaseGeneration()
			return nil, problem
		}
		response.Body.Close()
		finish()
		r.observeAttemptUsage(req.AuthID, body)
		if nativeHasUnfinishedToolCalls(body) {
			return nil, interrupted("bps_invalid_tool_response", "The upstream did not complete the response containing a tool call.")
		}
		if autoHistoryConflict(source, body) {
			return nil, failure(http.StatusUnprocessableEntity, "bps_duplicate_tool_call", "The upstream reused an existing tool call identifier.")
		}
		transformed, transformErr := bridge.Transform(body)
		if transformErr != nil {
			if compatible, selected, ok := bpsprotocol.DecodeAlternateToolResponse(source, body, bpsCallerScope(req), bpsProtocolOptions(cfg)); ok {
				transformed, mode, transformErr = compatible, selected, nil
			}
		}
		if transformErr != nil {
			if (incremental == nil || !incremental.activated) && attempt+1 < maxAutomaticAttempts && bpsprotocol.CanFallbackToolResponse(body, transformErr) && !autoHistoryConflict(source, body) {
				if !switched {
					next := "native"
					if mode == "native" {
						next = "text"
					}
					nextPayload, nextBridge, prepareErr := prepare(next)
					if prepareErr != nil {
						return nil, protocolError(prepareErr)
					}
					earlier = append(earlier, body)
					mode, payload, bridge = next, nextPayload, nextBridge
					switched = true
					continue
				}
				if mode == "native" && repairs < cfg.NativeToolRepairAttempts {
					if next, ok := bpsprotocol.RepairNativePayload(payload, body, transformErr); ok {
						earlier = append(earlier, body)
						payload = next
						repairs++
						continue
					}
				}
			}
			return nil, protocolError(transformErr)
		}
		if autoHistoryConflict(source, transformed) {
			return nil, failure(http.StatusUnprocessableEntity, "bps_duplicate_tool_call", "The upstream reused an existing tool call identifier.")
		}
		if err := tools.ValidateResponse(transformed); err != nil {
			return nil, interrupted("bps_invalid_tool_response", "The upstream returned a tool call outside the declared contract.")
		}
		final, mergeErr := mergeObservedUsage(transformed, earlier)
		if mergeErr != nil {
			return nil, interrupted("bps_invalid_usage", "The upstream usage metadata could not be normalized.")
		}
		if incremental != nil && incremental.activated {
			if err := incremental.prepareTerminal(final); err != nil {
				return nil, interrupted("bps_invalid_tool_response", "The final response contradicted its delivered text or output order.")
			}
		}
		var terminal struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(final, &terminal)
		if terminal.Status == "completed" {
			r.transports.learn(binding, mode)
			r.observeCompleted(req.AuthID)
		}
		return final, nil
	}
	return nil, interrupted("bps_tool_protocol_unavailable", "The upstream did not satisfy the tool protocol within the bounded compatibility attempts.")
}

func autoHistoryConflict(source, response []byte) bool {
	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(source, &request) != nil {
		return true
	}
	if trimmed := bytes.TrimSpace(request.Input); len(trimmed) > 0 && trimmed[0] == '"' {
		return false
	}
	var input []struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
	}
	if len(request.Input) > 0 && json.Unmarshal(request.Input, &input) != nil {
		return true
	}
	ids := map[string]bool{}
	for _, item := range input {
		if item.Type != "function_call" && item.Type != "custom_tool_call" && item.Type != "function_call_output" && item.Type != "custom_tool_call_output" {
			continue
		}
		if item.ID != "" {
			ids[item.ID] = true
		}
		if item.CallID != "" {
			ids[item.CallID] = true
		}
	}
	var result struct {
		Output []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if json.Unmarshal(response, &result) != nil {
		return true
	}
	for _, item := range result.Output {
		if item.Type != "function_call" && item.Type != "custom_tool_call" {
			continue
		}
		if ids[item.ID] || ids[item.CallID] {
			return true
		}
	}
	return false
}

// Optional malformed usage cannot invalidate a verified answer. Omit the
// incomplete total and mark it unknown instead of inventing a charge.
func mergeObservedUsage(final []byte, earlier [][]byte) ([]byte, error) {
	merged, err := bpsprotocol.MergeResponseUsage(final, earlier)
	if err == nil {
		return merged, nil
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(final, &response) != nil || response == nil {
		return nil, err
	}
	delete(response, "usage")
	var metadata map[string]json.RawMessage
	if json.Unmarshal(response["metadata"], &metadata) != nil || metadata == nil {
		metadata = map[string]json.RawMessage{}
	}
	metadata["cpa_bps_usage_complete"] = json.RawMessage(`"false"`)
	count, _ := json.Marshal(fmt.Sprint(len(earlier) + 1))
	metadata["cpa_bps_attempts"] = count
	response["metadata"], _ = json.Marshal(metadata)
	return json.Marshal(response)
}

// Failed compatibility attempts consume tokens but do not count as successful
// client requests. Record observed numeric counters even when fallback fails.
func (r *Runtime) observeAttemptUsage(id string, body []byte) {
	var response struct {
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.quotaEntry(id)
	entry.input = addUsage(entry.input, response.Usage.Input)
	entry.output = addUsage(entry.output, response.Usage.Output)
}
func (r *Runtime) observeCompleted(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.quotaEntry(id)
	entry.completed = addUsage(entry.completed, 1)
}
