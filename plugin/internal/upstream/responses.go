package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxJSONBytes  = 16 << 20
	maxEventBytes = 8 << 20
)

var (
	ErrResponseTooLarge = errors.New("upstream response exceeded the size limit")
	ErrInvalidJSON      = errors.New("upstream returned invalid JSON")
	ErrInvalidEvent     = errors.New("upstream sent an invalid Responses event")
	ErrStreamIncomplete = errors.New("upstream stream ended before a terminal response")
	ErrUpstreamEvent    = errors.New("upstream reported a terminal error event")
)

func ReadJSON(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxJSONBytes {
		return nil, ErrResponseTooLarge
	}
	if !json.Valid(data) {
		return nil, ErrInvalidJSON
	}
	return data, nil
}

// ReadEvents publishes complete data frames and returns the raw response from a
// terminal Responses event. A nil emit callback aggregates without streaming.
// The caller should pass a reader that unblocks if ctx is canceled.
func ReadEvents(ctx context.Context, body io.Reader, emit func(frame []byte) error) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32<<10), maxEventBytes+2)
	scanner.Split(scanLine)
	var frame eventFrame
	first := true
	for ctx.Err() == nil && scanner.Scan() {
		line := scanner.Bytes()
		if first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			first = false
		}
		if len(line) > 0 {
			if err := frame.addLine(line); err != nil {
				return nil, err
			}
			continue
		}
		response, err := frame.dispatch(emit)
		if err != nil || response != nil {
			return response, err
		}
		frame.reset()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scanErr := scanner.Err()
	if errors.Is(scanErr, ErrResponseTooLarge) {
		return nil, ErrResponseTooLarge
	}
	// Some Responses servers close cleanly without a final blank line. An
	// interrupted read does not get the same treatment as a clean EOF.
	if scanErr == nil {
		response, err := frame.dispatch(emit)
		if err != nil || response != nil {
			return response, err
		}
	}
	return nil, ErrStreamIncomplete
}

func scanLine(data []byte, atEOF bool) (int, []byte, error) {
	advance, token, err := bufio.ScanLines(data, atEOF)
	if len(token) > maxEventBytes || token == nil && len(data) >= maxEventBytes+2 {
		return 0, nil, ErrResponseTooLarge
	}
	return advance, token, err
}

type eventFrame struct {
	name   string
	data   bytes.Buffer
	fields int
}

func (f *eventFrame) addLine(line []byte) error {
	field, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "event":
		if len(value)+f.data.Len() > maxEventBytes {
			return ErrResponseTooLarge
		}
		if !utf8.Valid(value) || bytes.ContainsFunc(value, unicode.IsControl) {
			return ErrInvalidEvent
		}
		f.name = string(value)
	case "data":
		if len(value)+f.data.Len()+len(f.name)+1 > maxEventBytes {
			return ErrResponseTooLarge
		}
		if f.fields != 0 {
			f.data.WriteByte('\n')
		}
		f.data.Write(value)
		f.fields++
	}
	return nil
}

func (f *eventFrame) reset() {
	f.name = ""
	f.data.Reset()
	f.fields = 0
}

func (f *eventFrame) dispatch(emit func([]byte) error) (json.RawMessage, error) {
	if f.fields == 0 {
		return nil, nil
	}
	data := f.data.Bytes()
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, ErrStreamIncomplete
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrInvalidEvent
	}
	var event struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, ErrInvalidEvent
	}
	missingType := event.Type == ""
	if missingType {
		event.Type = f.name
	}
	if event.Type == "" || event.Type == "message" || strings.ContainsFunc(event.Type, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return nil, ErrInvalidEvent
	}
	var result json.RawMessage
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		response := bytes.TrimSpace(event.Response)
		if len(response) == 0 || response[0] != '{' {
			return nil, ErrInvalidEvent
		}
		result = event.Response
	}
	if emit != nil {
		if missingType {
			var err error
			data, err = withEventType(data, event.Type)
			if err != nil {
				return nil, err
			}
		}
		if err := emit(f.encode(data, event.Type)); err != nil {
			return nil, err
		}
	}
	if event.Type == "error" {
		return nil, ErrUpstreamEvent
	}
	return result, nil
}

func withEventType(data []byte, kind string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, ErrInvalidEvent
	}
	fields["type"], _ = json.Marshal(kind)
	result, err := json.Marshal(fields)
	if err != nil {
		return nil, ErrInvalidEvent
	}
	if len(result) > maxEventBytes {
		return nil, ErrResponseTooLarge
	}
	return result, nil
}

func (f *eventFrame) encode(data []byte, kind string) []byte {
	var normalized bytes.Buffer
	if f.name != "" {
		normalized.WriteString("event: ")
		normalized.WriteString(kind)
		normalized.WriteByte('\n')
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		normalized.WriteString("data: ")
		normalized.Write(line)
		normalized.WriteByte('\n')
	}
	normalized.WriteByte('\n')
	return normalized.Bytes()
}
