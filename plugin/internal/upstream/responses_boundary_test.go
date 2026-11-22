package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

type slicedEventReader struct {
	reader io.Reader
	size   int
}

type repeatedEventReader struct {
	frame     []byte
	remaining int
	position  int
}

func (r *repeatedEventReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.frame[r.position:])
	r.position += n
	if r.position == len(r.frame) {
		r.position = 0
		r.remaining--
	}
	return n, nil
}

func TestReadEventsLargeCumulativeStreamDoesNotRequireAggregation(t *testing.T) {
	// Generate more than the JSON body limit without storing the stream in
	// memory. Event framing must stay incremental; only the terminal response
	// is aggregated when emit is nil or callbacks discard their frames.
	frame := []byte(`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 4096) + "\"}\n\n")
	const count = 5000
	if len(frame)*count <= maxJSONBytes {
		t.Fatal("fixture does not cross the cumulative JSON limit")
	}
	const terminal = `data: {"type":"response.completed","response":{"id":"resp-large-stream","status":"completed","output":[]}}` + "\n\n"
	for _, streaming := range []bool{false, true} {
		emitted := 0
		var emit func([]byte) error
		if streaming {
			emit = func([]byte) error { emitted++; return nil }
		}
		reader := io.MultiReader(&repeatedEventReader{frame: frame, remaining: count}, strings.NewReader(terminal))
		body, err := ReadEvents(context.Background(), reader, emit)
		if err != nil || string(body) != `{"id":"resp-large-stream","status":"completed","output":[]}` || streaming && emitted != count+1 {
			t.Fatalf("large incremental stream failed: streaming=%v emitted=%d body=%s err=%v", streaming, emitted, body, err)
		}
	}
}

func (r slicedEventReader) Read(p []byte) (int, error) {
	return r.reader.Read(p[:min(len(p), r.size)])
}

func TestReadEventsAcceptsAllSSELineEndingsAndReadBoundaries(t *testing.T) {
	lines := []string{": heartbeat", "", "event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"hello"}`, "", "event: response.completed", `data: {"type":"response.completed","response":{"id":"resp-lines","status":"completed","output":[]}}`, ""}
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for size := 1; size <= 64; size++ {
			raw := "\xef\xbb\xbf" + strings.Join(lines, ending) + ending
			frames := 0
			body, err := ReadEvents(context.Background(), slicedEventReader{strings.NewReader(raw), size}, func([]byte) error { frames++; return nil })
			if err != nil || frames != 2 || string(body) != `{"id":"resp-lines","status":"completed","output":[]}` {
				t.Fatalf("line ending=%q chunk=%d returned body=%s frames=%d err=%v", ending, size, body, frames, err)
			}
		}
	}
}

func TestReadEventsCROnlyTerminalDoesNotWaitForAnotherByte(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := ReadEvents(context.Background(), reader, nil)
		done <- err
	}()
	if _, err := io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\r\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a complete CR-only terminal waited for EOF or another network byte")
	}
}

func FuzzReadEventsLineEndingChunkIndependence(f *testing.F) {
	f.Add([]byte{0, 1, 2, 0, 2}, uint8(1), "Hello 世界")
	f.Add([]byte{2}, uint8(7), "input_text")
	f.Add([]byte{1}, uint8(31), "quoted\"value\\path")
	f.Fuzz(func(t *testing.T, choices []byte, chunk uint8, text string) {
		if len(choices) == 0 || len(text) > 1024 {
			return
		}
		delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
		terminal := `{"type":"response.completed","response":{"id":"resp-fuzz","status":"completed","output":[]}}`
		blocks := [][]string{{": heartbeat"}, {"data: " + string(delta)}, {"event: response.completed", "data: " + terminal}}
		endings := []string{"\n", "\r\n", "\r"}
		var wire bytes.Buffer
		for i, lines := range blocks {
			ending := endings[int(choices[i%len(choices)])%len(endings)]
			wire.WriteString(strings.Join(lines, ending))
			wire.WriteString(ending + ending)
		}
		var frames [][]byte
		body, err := ReadEvents(context.Background(), slicedEventReader{bytes.NewReader(wire.Bytes()), 1 + int(chunk)%64}, func(frame []byte) error {
			frames = append(frames, bytes.Clone(frame))
			return nil
		})
		if err != nil || len(frames) != 2 || string(body) != `{"id":"resp-fuzz","status":"completed","output":[]}` {
			t.Fatalf("line ending/chunk segmentation changed semantics: body=%s frames=%d err=%v", body, len(frames), err)
		}
	})
}
