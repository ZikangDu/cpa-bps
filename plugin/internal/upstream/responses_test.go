package upstream

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestReadEventsIncremental(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	frames := make(chan string, 2)
	type outcome struct {
		body string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		defer reader.Close()
		body, err := ReadEvents(context.Background(), reader, func(frame []byte) error {
			frames <- string(frame)
			return nil
		})
		done <- outcome{body: string(body), err: err}
	}()
	if _, err := io.WriteString(writer, "\xef\xbb\xbf: keepalive\r\n\r\n"+
		"event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\r\n"+
		"data: \"delta\":\"hello\"}\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	want := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n"
	select {
	case got := <-frames:
		if got != want {
			t.Fatalf("first frame = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delta was not emitted before the terminal frame arrived")
	}
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\"}}\n\n"
	if _, err := io.WriteString(writer, terminal); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.body != `{"id":"resp_test"}` {
			t.Fatalf("terminal response = %q, error = %v", outcome.body, outcome.err)
		}
		select {
		case got := <-frames:
			if got != terminal {
				t.Fatalf("terminal frame = %q, want %q", got, terminal)
			}
		default:
			t.Fatal("terminal frame was not emitted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader kept waiting after the terminal frame")
	}
}

func TestReadEventsTerminalResponses(t *testing.T) {
	for _, kind := range []string{"completed", "failed", "incomplete"} {
		status := `{"status":"` + kind + `"}`
		raw := `data: {"type":"response.` + kind + `","response":` + status + "}\n\n"
		body, err := ReadEvents(context.Background(), strings.NewReader(raw), nil)
		if err != nil || string(body) != status {
			t.Fatalf("terminal response = %q, want %q (error %v)", body, status, err)
		}
	}
}

func TestReadEventsTerminalWithoutBlankLine(t *testing.T) {
	const final = `data: {"type":"response.completed","response":{"id":"resp_eof"}}`
	for _, ending := range []string{"", "\n", "\r\n"} {
		var frame string
		body, err := ReadEvents(context.Background(), strings.NewReader(final+ending), func(value []byte) error {
			frame = string(value)
			return nil
		})
		if err != nil || string(body) != `{"id":"resp_eof"}` || frame != final+"\n\n" {
			t.Fatalf("ending %q returned body %q, frame %q, error %v", ending, body, frame, err)
		}
	}
	reader := io.MultiReader(strings.NewReader(final), iotest.ErrReader(io.ErrUnexpectedEOF))
	body, err := ReadEvents(context.Background(), reader, func(_ []byte) error {
		t.Error("unterminated terminal frame was emitted after a read failure")
		return nil
	})
	if body != nil || !errors.Is(err, ErrStreamIncomplete) {
		t.Fatalf("read failure returned body %q, error %v", body, err)
	}
}

func TestReadEventsTypeFromName(t *testing.T) {
	const first = "event: response.output_text.delta\ndata: {\"delta\":\"hello\",\"extra\":[true,2]}\n\n"
	const canonical = "event: vendor.wrapper\ndata: {\"type\": \"vendor.unknown\", \"payload\": {\"future\": true}}\n\n"
	const final = "event: response.completed\ndata: {\"response\":{\"id\":\"resp_named\"}}"
	var frames []string
	body, err := ReadEvents(context.Background(), strings.NewReader(first+canonical+final), func(value []byte) error {
		frames = append(frames, string(value))
		return nil
	})
	want := []string{
		"event: response.output_text.delta\ndata: {\"delta\":\"hello\",\"extra\":[true,2],\"type\":\"response.output_text.delta\"}\n\n",
		strings.Replace(canonical, "event: vendor.wrapper", "event: vendor.unknown", 1),
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_named\"},\"type\":\"response.completed\"}\n\n",
	}
	if err != nil || string(body) != `{"id":"resp_named"}` || !slices.Equal(frames, want) {
		t.Fatalf("named events returned body %q, frames %q, error %v", body, frames, err)
	}
}

func TestReadEventsErrors(t *testing.T) {
	for _, item := range []struct {
		name   string
		raw    string
		frames int
		want   error
	}{
		{"early EOF", "data: {\"type\":\"response.created\"}\n\n", 1, ErrStreamIncomplete},
		{"unfinished terminal", "data: {\"type\":\"response.completed\",\"response\":{}\n", 0, ErrInvalidEvent},
		{"named invalid payload", "event: response.created\ndata: null\n\n", 0, ErrInvalidEvent},
		{"name with control character", "event: invalid\x00name\ndata: {}\n\n", 0, ErrInvalidEvent},
		{"name with whitespace", "event: invalid name\ndata: {}\n\n", 0, ErrInvalidEvent},
		{"type with control character", "event: safe\ndata: {\"type\":\"bad\\nname\"}\n\n", 0, ErrInvalidEvent},
		{"early DONE", "data: [DONE]\n\n", 0, ErrStreamIncomplete},
		{"invalid JSON", "data: }\n\n", 0, ErrInvalidEvent},
		{"invalid terminal", "data: {\"type\":\"response.completed\",\"response\":null}\n\n", 0, ErrInvalidEvent},
		{"upstream error", "data: {\"type\":\"error\",\"message\":\"failed\"}\n\n", 1, ErrUpstreamEvent},
		{"oversized line", "data:" + strings.Repeat("x", maxEventBytes) + "\n\n", 0, ErrResponseTooLarge},
		{"oversized frame", "data:" + strings.Repeat("x", maxEventBytes-10) + "\ndata:0123456789\n\n", 0, ErrResponseTooLarge},
	} {
		t.Run(item.name, func(t *testing.T) {
			frames := 0
			_, err := ReadEvents(context.Background(), strings.NewReader(item.raw), func(_ []byte) error {
				frames++
				return nil
			})
			if !errors.Is(err, item.want) || frames != item.frames {
				t.Fatalf("emitted %d frames (want %d), got error %v, want %v", frames, item.frames, err, item.want)
			}
		})
	}
}

func TestReadJSON(t *testing.T) {
	if got, err := ReadJSON(strings.NewReader(`{"ok":true}`)); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("ReadJSON returned %q, error %v", got, err)
	}
	if _, err := ReadJSON(strings.NewReader(`{`)); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("invalid JSON error = %v", err)
	}
	if _, err := ReadJSON(strings.NewReader(strings.Repeat(" ", maxJSONBytes+1))); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("oversized JSON error = %v", err)
	}
}
