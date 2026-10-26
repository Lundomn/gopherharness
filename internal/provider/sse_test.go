package provider

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEMultilineAndLineEndings(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		source := "\ufeff: heartbeat\nevent: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"model\":\"m\"}}\n\ndata: [DONE]\n\n"
		source = strings.ReplaceAll(source, "\n", newline)
		calls := 0
		err := readEventStream(strings.NewReader(source), func(raw []byte) error {
			calls++
			if !json.Valid(raw) || !strings.Contains(string(raw), "\n") {
				t.Fatalf("bad multiline payload: %q", raw)
			}
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("newline=%q calls=%d err=%v", newline, calls, err)
		}
	}
}

func TestSSERejectsUnterminatedOrOversizeEvent(t *testing.T) {
	calls := 0
	err := readEventStream(strings.NewReader("data: {\"tool\":\"write\"}\n"), func([]byte) error { calls++; return nil })
	if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	err = readEventStream(strings.NewReader(strings.Repeat(line, 5)+"\n"), func([]byte) error { calls++; return nil })
	if err == nil || calls != 0 {
		t.Fatalf("oversize accepted: calls=%d err=%v", calls, err)
	}
	// Comment-only streams are bounded too, without allocating a 32 MiB payload.
	err = readEventStream(io.MultiReader(strings.NewReader(strings.Repeat(":"+strings.Repeat("x", 1022)+"\n", (maxStreamBytes/1024)+1))), func([]byte) error { return nil })
	if err == nil {
		t.Fatal("unbounded comments accepted")
	}
}
