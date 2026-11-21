package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/compat"
	"cpa-bps-reconstruction/internal/upstream"
)

// Native repair holds the whole response until the tool contract validates.
// No client receives a speculative Office call or text from a discarded turn.
// The only extra generation is a correction to an unexecuted native relay;
// transport/HTTP failures leave this loop immediately for normal host policy.
func (r *Runtime) executeNativeBuffered(cfg config, req executionRequest, payload []byte, tools compat.ToolCatalog, bridge *bpsprotocol.Bridge, stream bool) (any, error) {
	ctx, cancel := context.WithTimeout(r.ctx, executionWindow)
	defer cancel()
	req.parentContext = ctx
	transferred := false
	defer func() {
		if !transferred {
			r.tasks.Done()
		}
	}()
	var earlier [][]byte
	for attempt := 0; attempt <= cfg.NativeToolRepairAttempts; attempt++ {
		response, ctx, finish, err := r.open(cfg, req, payload, stream)
		if err != nil {
			return nil, err
		}
		kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		var body []byte
		if kind == eventStream {
			body, err = upstream.ReadEvents(ctx, response.Body, nil)
		} else {
			body, err = upstream.ReadJSON(response.Body)
		}
		if err != nil {
			problem := resultError(ctx, err, "bps_stream_interrupted")
			response.Body.Close()
			finish()
			return nil, problem
		}
		response.Body.Close()
		finish()
		r.observeAttemptUsage(req.AuthID, body)
		if nativeHasUnfinishedToolCalls(body) {
			return nil, interrupted("bps_invalid_tool_response", "The upstream did not complete the response containing a tool call.")
		}
		transformed, transformErr := bridge.Transform(body)
		if transformErr != nil {
			if attempt < cfg.NativeToolRepairAttempts {
				if next, ok := bpsprotocol.RepairNativePayload(payload, body, transformErr); ok {
					earlier = append(earlier, body)
					payload = next
					continue
				}
			}
			return nil, protocolError(transformErr)
		}
		if err := tools.ValidateResponse(transformed); err != nil {
			return nil, interrupted("bps_invalid_tool_response", "the upstream returned a tool call outside the request contract")
		}
		transformed, err = mergeObservedUsage(transformed, earlier)
		if err != nil {
			return nil, interrupted("bps_invalid_usage", "The upstream usage metadata could not be normalized.")
		}
		var terminal struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(transformed, &terminal)
		if terminal.Status == "completed" {
			r.observeCompleted(req.AuthID)
		}
		if !stream {
			return map[string]any{"Payload": []byte(transformed), "Headers": http.Header{"Content-Type": {"application/json"}}}, nil
		}
		frames, err := bpsprotocol.ResponseStream(transformed)
		if err != nil {
			return nil, protocolError(err)
		}
		transferred = true
		go func() {
			defer r.tasks.Done()
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
	return nil, failure(http.StatusBadGateway, "bps_native_tool_repair_exhausted", "The native tool relay could not be corrected.")
}

// A terminal event can still represent an incomplete or failed response.
// Never turn its provisional calls into executable synthetic done events.
func nativeHasUnfinishedToolCalls(raw []byte) bool {
	var response struct {
		Status string `json:"status"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Status == "completed" {
		return false
	}
	for _, item := range response.Output {
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			return true
		}
	}
	return false
}
