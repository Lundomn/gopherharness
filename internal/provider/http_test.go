package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
)

func TestOpenAIAndAnthropicClients(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/responses" {
				t.Errorf("path=%s", r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer key" {
				t.Error("missing auth")
			}
			fmt.Fprint(w, `{"output_text":"<final>ok</final>","usage":{"input_tokens":3,"output_tokens":2}}`)
		}))
		defer srv.Close()
		c := NewOpenAI(config.Provider{Name: "openai", Protocol: "openai", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10)
		out, err := c.Complete(context.Background(), "hi", 10)
		if err != nil || out.Text != "<final>ok</final>" || out.Usage.InputTokens != 3 {
			t.Fatalf("out=%#v err=%v", out, err)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/messages" {
				t.Errorf("path=%s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") != "key" {
				t.Error("missing key")
			}
			fmt.Fprint(w, `{"content":[{"type":"text","text":"<final>ok</final>"}],"usage":{"input_tokens":4,"output_tokens":1}}`)
		}))
		defer srv.Close()
		c := NewAnthropic(config.Provider{Name: "deepseek", Protocol: "anthropic", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10)
		out, err := c.Complete(context.Background(), "hi", 10)
		if err != nil || out.Text != "<final>ok</final>" || out.Usage.OutputTokens != 1 {
			t.Fatalf("out=%#v err=%v", out, err)
		}
	})
}

func TestVisionRequests(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), "data:image/png;base64,iVBORw==") {
				t.Fatalf("missing image payload: %s", raw)
			}
			fmt.Fprint(w, `{"output_text":"a tiny image"}`)
		}))
		defer srv.Close()
		c := NewOpenAI(config.Provider{Name: "openai", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10)
		vision := c.(VisionClient)
		out, err := vision.InspectImage(context.Background(), "image/png", []byte{0x89, 'P', 'N', 'G'}, "describe", 50)
		if err != nil || out.Text != "a tiny image" {
			t.Fatalf("out=%#v err=%v", out, err)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), `"media_type":"image/jpeg"`) {
				t.Fatalf("missing image payload: %s", raw)
			}
			fmt.Fprint(w, `{"content":[{"type":"text","text":"a photo"}]}`)
		}))
		defer srv.Close()
		c := NewAnthropic(config.Provider{Name: "anthropic", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10)
		out, err := c.(VisionClient).InspectImage(context.Background(), "image/jpeg", []byte{1, 2, 3}, "describe", 50)
		if err != nil || out.Text != "a photo" {
			t.Fatalf("out=%#v err=%v", out, err)
		}
	})
}

func TestStreamingClients(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"<final>hel"}`)
			fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"lo</final>"}`)
			fmt.Fprintln(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":3}}}`)
			fmt.Fprintln(w, `data: [DONE]`)
		}))
		defer srv.Close()
		client := NewOpenAI(config.Provider{Name: "openai", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10).(StreamClient)
		var deltas strings.Builder
		out, err := client.Stream(context.Background(), "hi", 20, func(delta string) error { deltas.WriteString(delta); return nil })
		if err != nil || out.Text != "<final>hello</final>" || deltas.String() != out.Text || out.Usage.InputTokens != 7 {
			t.Fatalf("out=%#v deltas=%q err=%v", out, deltas.String(), err)
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`)
			fmt.Fprintln(w, `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"<final>ok</final>"}}`)
			fmt.Fprintln(w, `data: {"type":"message_delta","usage":{"output_tokens":2}}`)
		}))
		defer srv.Close()
		client := NewAnthropic(config.Provider{Name: "anthropic", APIKey: "key", BaseURL: srv.URL, Model: "m"}, 10).(StreamClient)
		out, err := client.Stream(context.Background(), "hi", 20, func(string) error { return nil })
		if err != nil || out.Text != "<final>ok</final>" || out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 2 {
			t.Fatalf("out=%#v err=%v", out, err)
		}
	})
}
