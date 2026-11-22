package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	"cpa-bps-reconstruction/internal/compat"
	"cpa-bps-reconstruction/internal/upstream"
)

const maxAutomaticPrefixBytes = 1 << 20
const maxAutomaticPrefixFrames = 1024
const maxAutomaticTextBytes = 8 << 20
const maxAutomaticTextItems = 4096

type automaticMessage struct {
	index int
	final bool
	done  bool
	parts map[int]*automaticTextProgress
}

type automaticTextProgress struct {
	text     strings.Builder
	textDone bool
	partDone bool
}

// autoTextStream only commits an explicitly marked native final-answer text
// prefix. Tools, unknown output kinds, commentary, text relays and structured
// replies keep the original complete-response validation path.
// All methods run on the single upstream reader goroutine. ready/resume transfer
// RPC ownership before the first host emission, avoiding a full stream-channel
// deadlock while ExecuteStream is still awaiting its RPC result.
type autoTextStream struct {
	ready           chan struct{}
	resume          chan struct{}
	activated       bool
	eligible        bool
	blocked         bool
	frames          [][]byte
	size            int
	messages        map[string]*automaticMessage
	indexOwners     map[int]string
	textBytes       int
	partCount       int
	fixups          [][]byte
	lastSequence    int64
	lastOutputIndex int
	terminal        []byte
	guard           func([]byte) error
}

func newAutoTextStream(tools compat.ToolCatalog, emit func([]byte) error) *autoTextStream {
	s := &autoTextStream{ready: make(chan struct{}), resume: make(chan struct{}), lastSequence: -1, lastOutputIndex: -1}
	s.guard = compat.GuardStream(tools, func(frame []byte) error {
		if err := emit(frame); err != nil {
			return err
		}
		if event, ok := automaticStreamEvent(frame); ok {
			if index, valid := automaticEventIndex(event["output_index"]); valid && index > s.lastOutputIndex {
				s.lastOutputIndex = index
			}
		}
		return nil
	})
	return s
}

func (s *autoTextStream) beginAttempt(eligible bool) {
	s.eligible, s.blocked = eligible, false
	s.frames, s.size = nil, 0
	s.messages = make(map[string]*automaticMessage)
	s.indexOwners = make(map[int]string)
	s.textBytes, s.partCount = 0, 0
	s.fixups = nil
	s.lastSequence = -1
	s.lastOutputIndex = -1
	s.terminal = nil
}

func automaticStreamEvent(frame []byte) (map[string]any, bool) {
	var data bytes.Buffer
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	return transportObject(data.Bytes())
}

// JSON integer fields may use decimal/exponent notation. Normalize exactly,
// without float underflow/rounding turning a fraction into an integer.
func automaticInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	if parsed, err := number.Int64(); err == nil {
		return parsed, true
	}
	raw := number.String()
	if len(raw) > 256 {
		return 0, false
	}
	negative := strings.HasPrefix(raw, "-")
	if negative {
		raw = raw[1:]
	}
	mantissa, exponent := raw, "0"
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		mantissa, exponent = raw[:index], raw[index+1:]
	}
	fraction := 0
	if index := strings.IndexByte(mantissa, '.'); index >= 0 {
		fraction = len(mantissa) - index - 1
		mantissa = mantissa[:index] + mantissa[index+1:]
	}
	if mantissa == "" {
		return 0, false
	}
	for _, digit := range mantissa {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return 0, true
	}
	exp, err := strconv.Atoi(exponent)
	if err != nil || exp > 4096 || exp < -4096 {
		return 0, false
	}
	scale := exp - fraction
	if scale < 0 {
		places := -scale
		if places >= len(digits) || strings.Trim(digits[len(digits)-places:], "0") != "" {
			return 0, false
		}
		digits = digits[:len(digits)-places]
	} else {
		if len(digits)+scale > 19 {
			return 0, false
		}
		digits += strings.Repeat("0", scale)
	}
	if negative {
		digits = "-" + digits
	}
	parsed, err := strconv.ParseInt(digits, 10, 64)
	return parsed, err == nil
}

func automaticEventIndex(value any) (int, bool) {
	parsed, ok := automaticInteger(value)
	if !ok || parsed < 0 || parsed >= maxAutomaticTextItems {
		return 0, false
	}
	return int(parsed), true
}

func canonicalAutomaticFrame(frame []byte, event map[string]any) ([]byte, error) {
	changed := false
	for _, field := range []string{"output_index", "content_index", "summary_index", "sequence_number"} {
		value, present := event[field]
		if !present {
			continue
		}
		parsed, ok := automaticInteger(value)
		if !ok || parsed < 0 || field != "sequence_number" && parsed >= maxAutomaticTextItems {
			return nil, compat.ErrInvalidToolResponse
		}
		canonical := strconv.FormatInt(parsed, 10)
		if value.(json.Number).String() != canonical {
			event[field] = json.Number(canonical)
			changed = true
		}
	}
	if !changed {
		return frame, nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, compat.ErrInvalidToolResponse
	}
	out := append([]byte("event: "+transportText(event["type"])+"\ndata: "), encoded...)
	return append(out, '\n', '\n'), nil
}

func (s *autoTextStream) accept(frame []byte) error {
	if !s.eligible || s.blocked && !s.activated {
		return nil
	}
	event, ok := automaticStreamEvent(frame)
	if !ok {
		return compat.ErrInvalidToolResponse
	}
	canonical, err := canonicalAutomaticFrame(frame, event)
	if err != nil {
		if s.activated {
			return err
		}
		s.disable()
		return nil
	}
	frame = canonical
	kind := transportText(event["type"])
	if sequence, ok := event["sequence_number"].(json.Number); ok {
		if value, err := sequence.Int64(); err == nil && value > s.lastSequence {
			s.lastSequence = value
		}
	}
	switch kind {
	case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
		// runAutomaticAttempts owns validation and emits exactly one final
		// response/error. Neither a provisional terminal nor a retry can leak.
		return nil
	}
	item := transportMap(event["item"])
	itemKind := transportText(item["type"])
	toolEvent := strings.HasPrefix(kind, "response.function_call_arguments.") || strings.HasPrefix(kind, "response.custom_tool_call_input.") || itemKind == "function_call" || itemKind == "custom_tool_call"
	if toolEvent {
		if index, valid := automaticEventIndex(event["output_index"]); valid && s.indexOwners[index] != "" {
			if s.activated {
				return compat.ErrInvalidToolResponse
			}
			s.disable()
			return nil
		}
		if s.activated {
			return s.guard(frame)
		}
		s.disable()
		return nil
	}
	if itemKind != "" && itemKind != "message" && itemKind != "reasoning" {
		if s.activated {
			return compat.ErrInvalidToolResponse
		}
		s.disable()
		return nil
	}
	if !automaticSafeEvent(kind) {
		if s.activated {
			return compat.ErrInvalidToolResponse
		}
		s.disable()
		return nil
	}
	if err := s.observeText(event); err != nil {
		if s.activated {
			return err
		}
		s.disable()
		return nil
	}
	if s.activated {
		return s.guard(frame)
	}
	if response := transportMap(event["response"]); response != nil {
		if output, ok := response["output"].([]any); ok && len(output) > 0 {
			s.disable()
			return nil
		}
	}
	if s.size+len(frame) > maxAutomaticPrefixBytes || len(s.frames) >= maxAutomaticPrefixFrames {
		s.disable()
		return nil
	}
	s.frames = append(s.frames, bytes.Clone(frame))
	s.size += len(frame)
	if kind != "response.output_text.delta" {
		return nil
	}
	id := transportText(event["item_id"])
	message := s.messages[id]
	index, valid := automaticEventIndex(event["output_index"])
	_, validPart := automaticEventIndex(event["content_index"])
	delta, text := event["delta"].(string)
	if message == nil || !message.final || !valid || index != message.index || !validPart || !text {
		s.disable()
		return nil
	}
	if delta == "" {
		return nil
	}
	prefix := strings.TrimSpace(delta)
	if prefix == "" {
		return nil
	}
	// Even in native mode an upstream can answer with a textual relay JSON
	// envelope. It must remain available to complete alternate decoding.
	if strings.ContainsRune("{[`", rune(prefix[0])) {
		s.disable()
		return nil
	}
	s.activated = true
	close(s.ready)
	<-s.resume
	frames := s.frames
	s.frames = nil
	s.size = 0
	for _, queued := range frames {
		if err := s.guard(queued); err != nil {
			return err
		}
	}
	return nil
}

func automaticSafeEvent(kind string) bool {
	switch kind {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done",
		"response.output_text.annotation.added", "response.refusal.delta", "response.refusal.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return true
	}
	return false
}

func (s *autoTextStream) disable() {
	s.blocked = true
	s.frames = nil
	s.size = 0
	s.messages = nil
	s.indexOwners = nil
	s.partCount = 0
	s.textBytes = 0
}

// Observe every text event, including suffix frames held by GuardStream. A
// provisional tool must not hide a later message identity/text contradiction.
func (s *autoTextStream) observeText(event map[string]any) error {
	kind := transportText(event["type"])
	item := transportMap(event["item"])
	if kind == "response.output_item.added" && transportText(item["type"]) == "message" {
		id := transportText(item["id"])
		index, valid := automaticEventIndex(event["output_index"])
		if id == "" || len(id) > 1024 || !valid || s.messages[id] != nil || s.indexOwners[index] != "" || len(s.messages) >= maxAutomaticTextItems {
			return compat.ErrInvalidToolResponse
		}
		if content, ok := item["content"].([]any); ok && len(content) > 0 {
			return compat.ErrInvalidToolResponse
		}
		s.messages[id] = &automaticMessage{index: index, final: transportText(item["phase"]) == "final_answer" && transportText(item["role"]) == "assistant", parts: make(map[int]*automaticTextProgress)}
		s.indexOwners[index] = id
		return nil
	}
	if kind == "response.output_item.done" && transportText(item["type"]) == "message" {
		message := s.messages[transportText(item["id"])]
		index, valid := automaticEventIndex(event["output_index"])
		if message == nil || !valid || index != message.index || message.done {
			return compat.ErrInvalidToolResponse
		}
		content, _ := item["content"].([]any)
		for partIndex, progress := range message.parts {
			if partIndex >= len(content) {
				return compat.ErrInvalidToolResponse
			}
			part := transportMap(content[partIndex])
			text, ok := part["text"].(string)
			if !ok || transportText(part["type"]) != "output_text" || text != progress.text.String() {
				return compat.ErrInvalidToolResponse
			}
		}
		for partIndex, raw := range content {
			part := transportMap(raw)
			if transportText(part["type"]) == "output_text" {
				text, ok := part["text"].(string)
				progress := message.parts[partIndex]
				if !ok || progress == nil || text != progress.text.String() {
					return compat.ErrInvalidToolResponse
				}
			}
		}
		message.done = true
		return nil
	}
	textEvent := kind == "response.output_text.delta" || kind == "response.output_text.done" || kind == "response.content_part.added" || kind == "response.content_part.done"
	if !textEvent {
		return nil
	}
	id := transportText(event["item_id"])
	message := s.messages[id]
	index, valid := automaticEventIndex(event["output_index"])
	partIndex, validPart := automaticEventIndex(event["content_index"])
	if message == nil || !valid || !validPart || index != message.index || message.done {
		return compat.ErrInvalidToolResponse
	}
	if kind == "response.content_part.added" || kind == "response.content_part.done" {
		part := transportMap(event["part"])
		if transportText(part["type"]) != "output_text" {
			return compat.ErrInvalidToolResponse
		}
	}
	if kind == "response.output_text.delta" {
		if delta, ok := event["delta"].(string); ok && delta == "" {
			return nil
		}
	}
	progress := message.parts[partIndex]
	if progress == nil {
		if s.partCount >= maxAutomaticTextItems {
			return compat.ErrInvalidToolResponse
		}
		progress = &automaticTextProgress{}
		message.parts[partIndex] = progress
		s.partCount++
	}
	switch kind {
	case "response.output_text.delta":
		delta, ok := event["delta"].(string)
		if !ok || progress.textDone || progress.partDone {
			return compat.ErrInvalidToolResponse
		}
		if len(delta) > maxAutomaticTextBytes-s.textBytes {
			return upstream.ErrResponseTooLarge
		}
		progress.text.WriteString(delta)
		s.textBytes += len(delta)
	case "response.output_text.done":
		text, ok := event["text"].(string)
		if !ok || text != progress.text.String() || progress.textDone || progress.partDone {
			return compat.ErrInvalidToolResponse
		}
		progress.textDone = true
	case "response.content_part.added":
		part := transportMap(event["part"])
		if transportText(part["text"]) != "" || progress.text.Len() > 0 || progress.partDone {
			return compat.ErrInvalidToolResponse
		}
	case "response.content_part.done":
		text, ok := transportMap(event["part"])["text"].(string)
		if !ok || text != progress.text.String() || progress.partDone {
			return compat.ErrInvalidToolResponse
		}
		progress.partDone = true
	}
	return nil
}

func (s *autoTextStream) prepareTerminal(raw []byte) error {
	response, valid := transportObject(raw)
	if !valid {
		return compat.ErrInvalidToolResponse
	}
	status := transportText(response["status"])
	if status != "completed" && status != "incomplete" {
		return compat.ErrInvalidToolResponse
	}
	output, ok := response["output"].([]any)
	if !ok {
		return compat.ErrInvalidToolResponse
	}
	found := make(map[string]bool)
	s.fixups = nil
	for outputIndex, rawItem := range output {
		item := transportMap(rawItem)
		kind := transportText(item["type"])
		if (kind == "function_call" || kind == "custom_tool_call") && outputIndex <= s.lastOutputIndex {
			return compat.ErrInvalidToolResponse
		}
		if kind != "message" {
			continue
		}
		id := transportText(item["id"])
		message := s.messages[id]
		if message == nil || message.index != outputIndex || found[id] {
			return compat.ErrInvalidToolResponse
		}
		found[id] = true
		content, ok := item["content"].([]any)
		if !ok {
			return compat.ErrInvalidToolResponse
		}
		for partIndex, rawPart := range content {
			part := transportMap(rawPart)
			if transportText(part["type"]) == "output_text" && message.parts[partIndex] == nil && transportText(part["text"]) != "" {
				// An unannounced text part cannot silently appear only in the
				// terminal after the caller has committed to incremental output.
				return compat.ErrInvalidToolResponse
			}
		}
		partIndexes := make([]int, 0, len(message.parts))
		for partIndex := range message.parts {
			partIndexes = append(partIndexes, partIndex)
		}
		sort.Ints(partIndexes)
		for _, partIndex := range partIndexes {
			progress := message.parts[partIndex]
			if partIndex >= len(content) {
				return compat.ErrInvalidToolResponse
			}
			part := transportMap(content[partIndex])
			text, ok := part["text"].(string)
			prefix := progress.text.String()
			if !ok || transportText(part["type"]) != "output_text" || !strings.HasPrefix(text, prefix) {
				return compat.ErrInvalidToolResponse
			}
			if len(text) > len(prefix) {
				if progress.textDone || progress.partDone || message.done {
					return compat.ErrInvalidToolResponse
				}
				if len(text)-len(prefix) > maxAutomaticTextBytes-s.textBytes {
					return upstream.ErrResponseTooLarge
				}
				s.textBytes += len(text) - len(prefix)
				s.addFixup(map[string]any{"type": "response.output_text.delta", "output_index": outputIndex, "content_index": partIndex, "item_id": id, "delta": text[len(prefix):]})
			}
		}
	}
	if len(found) != len(s.messages) {
		return compat.ErrInvalidToolResponse
	}
	event := map[string]any{"type": "response." + status, "response": json.RawMessage(raw)}
	s.terminal = s.eventFrame(event)
	return nil
}

func (s *autoTextStream) eventFrame(event map[string]any) []byte {
	if s.lastSequence >= 0 && s.lastSequence < math.MaxInt64 {
		s.lastSequence++
		event["sequence_number"] = s.lastSequence
	}
	encoded, _ := json.Marshal(event)
	frame := append([]byte("event: "+transportText(event["type"])+"\ndata: "), encoded...)
	return append(frame, '\n', '\n')
}
func (s *autoTextStream) addFixup(event map[string]any) {
	s.fixups = append(s.fixups, s.eventFrame(event))
}
func (s *autoTextStream) complete() error {
	if len(s.terminal) == 0 {
		return compat.ErrInvalidToolResponse
	}
	for _, frame := range s.fixups {
		if err := s.guard(frame); err != nil {
			return err
		}
	}
	return s.guard(s.terminal)
}

func (r *Runtime) closeAutomaticStream(streamID string, err error) {
	request := map[string]any{"stream_id": streamID}
	if err != nil {
		var detail *rpcError
		if !errors.As(err, &detail) {
			detail = resultError(context.Background(), err, "bps_stream_interrupted")
		}
		request["error_detail"] = map[string]any{"code": detail.Code(), "http_status": detail.StatusCode(), "message": detail.Error()}
	}
	_ = r.host("host.stream.close", request, nil)
}
