package compat

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

const (
	maxToolSuffixFrames = 8192
	maxToolSuffixBytes  = 8 << 20
)

type queuedEvent struct {
	data        json.RawMessage
	kind        string
	outputIndex *int
}

// GuardStream wraps the emitter for one normalized Responses SSE stream. It
// forwards the ordinary prefix incrementally and holds a bounded suffix after
// a provisional client tool appears. Verified tools are inserted into their
// output slots before later items are revealed.
func GuardStream(catalog ToolCatalog, emit func([]byte) error) func([]byte) error {
	var lastSequence int64 = -1
	lastOutputIndex := -1
	var pending bool
	var suffix []queuedEvent
	var suffixBytes int
	return func(frame []byte) error {
		var data bytes.Buffer
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if data.Len() != 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(value, []byte(" ")))
			}
		}
		if data.Len() == 0 {
			return emit(frame)
		}
		var event struct {
			Type        string          `json:"type"`
			Sequence    *int64          `json:"sequence_number"`
			OutputIndex *int            `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    json.RawMessage `json:"response"`
		}
		if json.Unmarshal(data.Bytes(), &event) != nil {
			return ErrInvalidToolResponse
		}
		if strings.HasPrefix(event.Type, "response.function_call_arguments.") ||
			strings.HasPrefix(event.Type, "response.custom_tool_call_input.") || isToolItem(event.Item) {
			pending = true
			return nil
		}
		if event.Type == "response.completed" {
			calls, err := catalog.validatedCalls(event.Response)
			if err != nil {
				return err
			}
			if len(calls) == 0 && len(suffix) == 0 {
				return emit(frame)
			}
			if len(calls) > 0 && calls[0].index <= lastOutputIndex {
				// This provider announced an earlier tool only in the terminal,
				// after exposing a later slot. Reordering an emitted slot is unsafe.
				return ErrInvalidToolResponse
			}
			sequence := eventSequence(event.Sequence, lastSequence, len(calls), len(suffix))
			if err := emitVerifiedSuffix(emit, calls, suffix, sequence); err != nil {
				return err
			}
			if sequence == nil {
				return emit(frame)
			}
			return emitRewrittenEvent(emit, data.Bytes(), event.Type, "sequence_number", *sequence)
		}
		failed := event.Type == "response.failed" || event.Type == "response.incomplete" ||
			event.Type == "response.error" || event.Type == "error"
		hold := pending && !failed
		payload := data.Bytes()
		var filtered json.RawMessage
		if !absent(event.Response) {
			var err error
			filtered, err = withoutProvisionalCalls(event.Response, !failed)
			if err != nil {
				return err
			}
			if filtered != nil {
				pending = true
				if hold {
					payload, err = rewriteEvent(payload, "response", filtered)
					if err != nil {
						return err
					}
				}
			}
		}
		if hold {
			if len(suffix) >= maxToolSuffixFrames || len(payload) > maxToolSuffixBytes-suffixBytes {
				return ErrInvalidToolResponse
			}
			suffix = append(suffix, queuedEvent{data: payload, kind: event.Type, outputIndex: event.OutputIndex})
			suffixBytes += len(payload)
			return nil
		}
		if event.Sequence != nil && *event.Sequence > lastSequence {
			lastSequence = *event.Sequence
		}
		if event.OutputIndex != nil && *event.OutputIndex > lastOutputIndex {
			lastOutputIndex = *event.OutputIndex
		}
		if filtered != nil {
			return emitRewrittenEvent(emit, payload, event.Type, "response", filtered)
		}
		return emit(frame)
	}
}

func isToolItem(raw json.RawMessage) bool {
	var item struct{ Type string }
	return json.Unmarshal(raw, &item) == nil && isClientToolCall(item.Type)
}

func withoutProvisionalCalls(raw json.RawMessage, prefixOnly bool) (json.RawMessage, error) {
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return nil, ErrInvalidToolResponse
	}
	if absent(response["output"]) {
		return nil, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(response["output"], &items) != nil {
		return nil, ErrInvalidToolResponse
	}
	kept := items[:0]
	for _, item := range items {
		if isToolItem(item) {
			if prefixOnly {
				// Progressive snapshots must keep slot indexes. A later item
				// cannot move into a still-unverified tool's position.
				break
			}
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == len(items) {
		return nil, nil
	}
	response["output"], _ = json.Marshal(kept)
	return json.Marshal(response)
}

func eventSequence(terminal *int64, last int64, calls, queued int) *int64 {
	if (terminal == nil && last < 0) || last == math.MaxInt64 || (terminal != nil && *terminal < 0) {
		return nil
	}
	start := last + 1
	if queued == 0 && terminal != nil {
		start = max(start, *terminal)
	}
	if start > math.MaxInt64-int64(4*calls+queued) {
		return nil
	}
	return &start
}

func emitVerifiedSuffix(emit func([]byte) error, calls []validatedCall, suffix []queuedEvent, sequence *int64) error {
	for _, event := range suffix {
		for len(calls) > 0 && event.outputIndex != nil && calls[0].index < *event.outputIndex {
			if err := emitVerifiedCall(emit, calls[0], sequence); err != nil {
				return err
			}
			calls = calls[1:]
		}
		if sequence == nil {
			if err := emitJSONEvent(emit, event.kind, event.data); err != nil {
				return err
			}
		} else {
			if err := emitRewrittenEvent(emit, event.data, event.kind, "sequence_number", *sequence); err != nil {
				return err
			}
			*sequence += 1
		}
	}
	for _, call := range calls {
		if err := emitVerifiedCall(emit, call, sequence); err != nil {
			return err
		}
	}
	return nil
}

func emitVerifiedCall(emit func([]byte) error, call validatedCall, sequence *int64) error {
	var added map[string]json.RawMessage
	if json.Unmarshal(call.item, &added) != nil || added == nil {
		return ErrInvalidToolResponse
	}
	added[call.argumentKey] = json.RawMessage(`""`)
	added["status"] = json.RawMessage(`"in_progress"`)
	for _, event := range []struct {
		kind   string
		fields map[string]any
	}{
		{"response.output_item.added", map[string]any{"item": added}},
		{call.eventPrefix + "delta", map[string]any{"item_id": call.id, "delta": call.argument}},
		{call.eventPrefix + "done", map[string]any{"item_id": call.id, call.argumentKey: call.argument}},
		{"response.output_item.done", map[string]any{"item": call.item}},
	} {
		if call.id == "" && strings.HasPrefix(event.kind, call.eventPrefix) {
			// Output item IDs are optional, unlike IDs in argument events.
			continue
		}
		event.fields["type"] = event.kind
		event.fields["output_index"] = call.index
		if sequence != nil {
			event.fields["sequence_number"] = *sequence
			*sequence += 1
		}
		if err := emitJSONEvent(emit, event.kind, event.fields); err != nil {
			return err
		}
	}
	return nil
}

func emitRewrittenEvent(emit func([]byte) error, raw json.RawMessage, kind, field string, value any) error {
	rewritten, err := rewriteEvent(raw, field, value)
	if err != nil {
		return err
	}
	return emitJSONEvent(emit, kind, rewritten)
}

func rewriteEvent(raw json.RawMessage, field string, value any) (json.RawMessage, error) {
	var event map[string]json.RawMessage
	if json.Unmarshal(raw, &event) != nil || event == nil {
		return nil, ErrInvalidToolResponse
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidToolResponse
	}
	event[field] = encoded
	return json.Marshal(event)
}

func emitJSONEvent(emit func([]byte) error, kind string, fields any) error {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ErrInvalidToolResponse
	}
	frame := []byte("event: " + kind + "\ndata: ")
	frame = append(frame, encoded...)
	frame = append(frame, '\n', '\n')
	return emit(frame)
}
