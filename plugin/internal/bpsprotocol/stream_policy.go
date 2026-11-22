package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// CanStreamPlainText identifies requests that cannot authorize a client tool
// or require a final structured value. Tool history alone does not change it.
// A stream validator must still withhold provisional tool events and enforce
// the terminal response; any published prefix disables generation replay.
func (b *Bridge) CanStreamPlainText() bool {
	return b != nil && b.structured == nil && !clientToolCallRequired(b.source) && len(callableClientToolSpecs(b.source)) == 0
}

// CanStreamNativeFinalText permits an adapter to expose only an explicit
// native final_answer text prefix. Required calls and text relay envelopes
// remain buffered. The adapter must retain one stream validator, preserve slot
// order and never regenerate or synthesize the full response after publication.
func (b *Bridge) CanStreamNativeFinalText() bool {
	return b != nil && b.structured == nil && !b.textTools && !clientToolCallRequired(b.source) && len(callableClientToolSpecs(b.source)) > 0
}

const (
	maxPlainTextPrefixFrames = 8192
	maxPlainTextPrefixBytes  = 8 << 20
)

// Text-history requests may still receive a forbidden client_tool JSON reply.
// Hold each message until its first nonblank text is ordinary prose. JSON-like
// messages hold the remaining suffix until Transform validates the response.
// Before publication a large prefix can revert to full terminal buffering.
type plainTextStreamGate struct {
	started, holding, blocked, dropped bool
	ordinary                           map[string]bool
	messageIDs                         map[int]string
	messageDone                        map[int]bool
	idIndexes                          map[string]int
	observed                           map[plainTextPart]*plainTextProgress
	observedBytes                      int
	lastSequence                       int64
	fixups                             []plainTextFixup
	pending                            [][]byte
	bytes                              int
}

type plainTextPart struct {
	output, content int
	id              string
}

type plainTextProgress struct {
	text               strings.Builder
	textDone, partDone bool
}

type plainTextFixup struct {
	part plainTextPart
	text string
}

func newPlainTextStreamGate() *plainTextStreamGate {
	return &plainTextStreamGate{ordinary: map[string]bool{}, messageIDs: map[int]string{}, messageDone: map[int]bool{}, idIndexes: map[string]int{}, observed: map[plainTextPart]*plainTextProgress{}, lastSequence: -1}
}

func (g *plainTextStreamGate) observe(kind string, event map[string]json.RawMessage) bool {
	if g.dropped {
		return true
	}
	if raw := event["sequence_number"]; len(raw) > 0 {
		var sequence int64
		if json.Unmarshal(raw, &sequence) != nil || sequence < 0 {
			return false
		}
		g.lastSequence = max(g.lastSequence, sequence)
	}
	var index int
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		var item struct {
			Type, ID string
			Content  []struct{ Type, Text string }
		}
		if json.Unmarshal(event["item"], &item) == nil && item.Type == "message" {
			if json.Unmarshal(event["output_index"], &index) != nil || index < 0 || index >= 4096 || item.ID == "" || len(item.ID) > 1024 {
				return false
			}
			if kind == "response.output_item.added" {
				if _, exists := g.messageIDs[index]; exists || len(item.Content) > 0 {
					return false
				}
				if _, exists := g.idIndexes[item.ID]; exists {
					return false
				}
				g.messageIDs[index], g.idIndexes[item.ID] = item.ID, index
				return true
			}
			if g.messageIDs[index] != item.ID || g.messageDone[index] {
				return false
			}
			for key, progress := range g.observed {
				if key.output == index && (key.content >= len(item.Content) || item.Content[key.content].Type != "output_text" || item.Content[key.content].Text != progress.text.String()) {
					return false
				}
			}
			for content, part := range item.Content {
				if part.Type == "output_text" {
					progress := g.observed[plainTextPart{index, content, item.ID}]
					if progress == nil && part.Text != "" || progress != nil && part.Text != progress.text.String() {
						return false
					}
				}
			}
			g.messageDone[index] = true
		}
	}
	if kind != "response.output_text.delta" && kind != "response.output_text.done" && kind != "response.content_part.added" && kind != "response.content_part.done" {
		return true
	}
	var part struct{ Type, Text string }
	if kind == "response.content_part.added" || kind == "response.content_part.done" {
		if json.Unmarshal(event["part"], &part) != nil {
			return false
		}
		if part.Type != "output_text" {
			return true
		}
	}
	var id, text string
	var content int
	if json.Unmarshal(event["output_index"], &index) != nil || index < 0 || index >= 4096 ||
		json.Unmarshal(event["content_index"], &content) != nil || content < 0 || content >= 4096 ||
		json.Unmarshal(event["item_id"], &id) != nil || id == "" || g.messageIDs[index] != id || g.messageDone[index] {
		return false
	}
	if kind == "response.output_text.delta" {
		if json.Unmarshal(event["delta"], &text) != nil {
			return false
		}
		if text == "" {
			return true
		}
	} else if kind == "response.output_text.done" {
		if json.Unmarshal(event["text"], &text) != nil {
			return false
		}
	} else {
		text = part.Text
	}
	key := plainTextPart{index, content, id}
	progress := g.observed[key]
	if progress == nil {
		if len(g.observed) >= 4096 {
			return false
		}
		progress = &plainTextProgress{}
		g.observed[key] = progress
	}
	switch kind {
	case "response.output_text.delta":
		if progress.textDone || progress.partDone || len(text) > maxPlainTextPrefixBytes-g.observedBytes {
			return false
		}
		progress.text.WriteString(text)
		g.observedBytes += len(text)
	case "response.output_text.done":
		if progress.textDone || progress.partDone || text != progress.text.String() {
			return false
		}
		progress.textDone = true
	case "response.content_part.added":
		if text != "" || progress.text.Len() > 0 || progress.partDone {
			return false
		}
	case "response.content_part.done":
		if progress.partDone || text != progress.text.String() {
			return false
		}
		progress.partDone = true
	}
	return true
}

func (g *plainTextStreamGate) validateTerminal(raw json.RawMessage) error {
	if !g.started {
		return nil
	}
	var response struct {
		Output []struct {
			Type, ID string
			Content  []struct{ Type, Text string }
		}
	}
	if json.Unmarshal(raw, &response) != nil {
		return fail(502, "bps_invalid_event", "BPS terminal text does not match its streamed prefix")
	}
	g.fixups = nil
	for output, item := range response.Output {
		if item.Type != "message" {
			continue
		}
		if g.messageIDs[output] != item.ID {
			return fail(502, "bps_invalid_event", "BPS terminal text does not match its streamed prefix")
		}
		for content, part := range item.Content {
			if part.Type == "output_text" && part.Text != "" && g.observed[plainTextPart{output, content, item.ID}] == nil {
				return fail(502, "bps_invalid_event", "BPS terminal text includes an unannounced content part")
			}
		}
	}
	for key, progress := range g.observed {
		if key.output >= len(response.Output) {
			return fail(502, "bps_invalid_event", "BPS terminal text does not match its streamed prefix")
		}
		item := response.Output[key.output]
		if item.Type != "message" || item.ID != key.id || key.content >= len(item.Content) ||
			item.Content[key.content].Type != "output_text" || !strings.HasPrefix(item.Content[key.content].Text, progress.text.String()) {
			return fail(502, "bps_invalid_event", "BPS terminal text does not match its streamed prefix")
		}
		if final := item.Content[key.content].Text; len(final) > progress.text.Len() {
			if progress.textDone || progress.partDone || g.messageDone[key.output] {
				return fail(502, "bps_invalid_event", "BPS terminal text changed an already completed content part")
			}
			g.fixups = append(g.fixups, plainTextFixup{key, final[progress.text.Len():]})
		}
	}
	sort.Slice(g.fixups, func(i, j int) bool {
		if g.fixups[i].part.output != g.fixups[j].part.output {
			return g.fixups[i].part.output < g.fixups[j].part.output
		}
		return g.fixups[i].part.content < g.fixups[j].part.content
	})
	return nil
}

// Only a validated terminal can supply missing text for an unfinished, already
// announced part. Add each suffix once, then keep a single terminal response.
func (g *plainTextStreamGate) emitFixups(event map[string]json.RawMessage, emit func([]byte) error) error {
	if len(g.fixups) == 0 {
		return nil
	}
	sequence := int64(-1)
	if g.lastSequence >= 0 {
		if g.lastSequence == math.MaxInt64 {
			return fail(502, "bps_invalid_event", "BPS event sequence cannot be extended")
		}
		sequence = g.lastSequence + 1
	}
	if raw := event["sequence_number"]; len(raw) > 0 {
		var terminal int64
		if json.Unmarshal(raw, &terminal) != nil || terminal < 0 {
			return fail(502, "bps_invalid_event", "BPS terminal sequence is invalid")
		}
		sequence = max(sequence, terminal)
	}
	if sequence > math.MaxInt64-int64(len(g.fixups))-1 {
		return fail(502, "bps_invalid_event", "BPS event sequence cannot be extended")
	}
	for _, fixup := range g.fixups {
		fields := map[string]any{"type": "response.output_text.delta", "output_index": fixup.part.output, "content_index": fixup.part.content, "item_id": fixup.part.id, "delta": fixup.text}
		if sequence >= 0 {
			fields["sequence_number"] = sequence
			sequence++
		}
		frame := []byte("event: response.output_text.delta\ndata: " + string(jsonBytes(fields)) + "\n\n")
		if err := emit(frame); err != nil {
			return err
		}
	}
	if sequence >= 0 {
		event["sequence_number"] = jsonBytes(sequence)
	}
	g.fixups = nil
	return nil
}

func (g *plainTextStreamGate) clear() {
	g.pending = nil
	g.bytes = 0
}

func (g *plainTextStreamGate) queue(frame []byte) error {
	if g.dropped {
		return nil
	}
	if len(g.pending) >= maxPlainTextPrefixFrames || len(frame) > maxPlainTextPrefixBytes-g.bytes {
		if g.started {
			return fail(502, "bps_invalid_event", "BPS buffered text suffix exceeds the size limit")
		}
		g.clear()
		g.blocked, g.dropped = true, true
		return nil
	}
	g.pending = append(g.pending, bytes.Clone(frame))
	g.bytes += len(frame)
	return nil
}

func (g *plainTextStreamGate) flush(emit func([]byte) error) error {
	defer g.clear()
	for _, frame := range g.pending {
		g.started = true
		if err := emit(frame); err != nil {
			return err
		}
	}
	return nil
}

func (g *plainTextStreamGate) frame(frame []byte, kind string, event map[string]json.RawMessage, emit func([]byte) error) error {
	if kind == "error" || kind == "response.error" || kind == "response.failed" {
		g.clear()
		return emit(frame)
	}
	if !g.observe(kind, event) {
		if g.started {
			return fail(502, "bps_invalid_event", "BPS text event identity is inconsistent")
		}
		g.blocked, g.dropped = true, true
		g.clear()
		g.observed = nil
		return nil
	}
	if g.blocked {
		return g.queue(frame)
	}
	// A populated progressive snapshot can contain unclassified text or tool
	// bodies. Keep it private until the complete terminal contract validates.
	if raw := event["response"]; len(raw) > 0 {
		var snapshot struct{ Output []json.RawMessage }
		if json.Unmarshal(raw, &snapshot) == nil && len(snapshot.Output) > 0 {
			g.blocked = true
			return g.queue(frame)
		}
	}
	key := string(event["output_index"])
	if key == "" {
		key = "unindexed"
	}
	var text string
	var hasText bool
	switch kind {
	case "response.output_text.delta":
		hasText = json.Unmarshal(event["delta"], &text) == nil
	case "response.output_text.done":
		hasText = json.Unmarshal(event["text"], &text) == nil
	case "response.content_part.added", "response.content_part.done":
		var part struct{ Type, Text string }
		if json.Unmarshal(event["part"], &part) == nil && part.Type == "output_text" {
			text, hasText = part.Text, true
		}
	case "response.output_item.added", "response.output_item.done":
		var item struct {
			Type    string
			Content []struct{ Type, Text string }
		}
		if json.Unmarshal(event["item"], &item) == nil && item.Type == "message" {
			if kind == "response.output_item.added" {
				delete(g.ordinary, key)
				g.holding = true
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
					hasText = true
				}
				if part.Type == "refusal" {
					g.blocked = true
				}
			}
		}
	}
	if hasText && !g.ordinary[key] {
		g.holding = true
	}
	if g.started && !g.holding && !g.blocked {
		return emit(frame)
	}
	if err := g.queue(frame); err != nil {
		return err
	}
	if g.blocked || !hasText || g.ordinary[key] {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if kind != "response.output_text.delta" {
		// Nonempty text snapshots are not incremental deltas and can differ
		// from later chunks. Preserve terminal buffering for those responses.
		g.blocked = true
		return nil
	}
	if strings.ContainsRune("{[`", rune(trimmed[0])) {
		g.blocked = true
		return nil
	}
	g.ordinary[key], g.holding = true, false
	return g.flush(emit)
}
