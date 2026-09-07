package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOneShotCLI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"output_text":"<final>CLI works</final>"}`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--cwd", t.TempDir(), "--provider", "openai", "--api-key", "key", "--base-url", srv.URL, "hello"}, Options{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "CLI works") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestFinalStreamOutputHidesProtocol(t *testing.T) {
	var out bytes.Buffer
	stream := &finalStreamOutput{out: &out}
	stream.Start()
	for _, delta := range []string{"<fi", "nal>hel", "lo</fi", "nal>"} {
		if err := stream.Delta(delta); err != nil {
			t.Fatal(err)
		}
	}
	if !stream.Finish("hello") {
		t.Fatal("stream was not rendered")
	}
	value := out.String()
	if !strings.Contains(value, "hello") || strings.Contains(value, "<final>") || strings.Contains(value, "</final>") {
		t.Fatalf("unexpected stream output: %q", value)
	}
}

func TestTUIStreamsFinalWithoutProtocolTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"<final>stream"}`)
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":" works</final>"}`)
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":2}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--cwd", t.TempDir(), "--provider", "openai", "--api-key", "key", "--base-url", srv.URL, "--no-auto-dream"}, Options{ForceTUI: true, Stdin: strings.NewReader("hello\n/exit\n"), Stdout: &stdout, Stderr: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "stream works") || strings.Contains(stdout.String(), "<final>") || strings.Contains(stdout.String(), "</final>") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
