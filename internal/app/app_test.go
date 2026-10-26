package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestContextProfileAndCLIOverrideReachProvider(t *testing.T) {
	for _, override := range []bool{false, true} {
		var gotOutput int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				MaxOutput int `json:"max_output_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			gotOutput = body.MaxOutput
			fmt.Fprint(w, `{"output_text":"<final>done</final>"}`)
		}))
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".pico.toml"), []byte("[providers.openai]\ncontext_window_tokens = 4096\nmax_output_tokens = 100\ntokenizer = \"o200k_base\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"--cwd", root, "--provider", "openai", "--base-url", srv.URL, "--api-key", "fake", "--no-auto-dream", "--history-selector", "rule"}
		want := 100
		if override {
			args = append(args, "--max-new-tokens", "160", "--context-window-tokens", "3000", "--context-tokenizer", "cl100k_base")
			want = 160
		}
		args = append(args, "finish")
		var out, errOut bytes.Buffer
		code := Run(args, Options{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut})
		srv.Close()
		if code != 0 || gotOutput != want {
			t.Fatalf("code=%d output=%d stderr=%s", code, gotOutput, errOut.String())
		}
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
		fmt.Fprintf(w, "%s\n\n", `data: {"type":"response.output_text.delta","delta":"<final>stream"}`)
		fmt.Fprintf(w, "%s\n\n", `data: {"type":"response.output_text.delta","delta":" works</final>"}`)
		fmt.Fprintf(w, "%s\n\n", `data: {"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":2}}}`)
		fmt.Fprintf(w, "%s\n\n", `data: [DONE]`)
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"--cwd", t.TempDir(), "--provider", "openai", "--api-key", "key", "--base-url", srv.URL, "--no-auto-dream"}, Options{ForceTUI: true, Stdin: strings.NewReader("hello\n/exit\n"), Stdout: &stdout, Stderr: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "stream works") || strings.Contains(stdout.String(), "<final>") || strings.Contains(stdout.String(), "</final>") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
