package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
)

func TestTextStreamRequiresTerminalEvent(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "openai" {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"partial\"}}\n\n")
				}
			}))
			defer srv.Close()
			p := config.Provider{Name: protocol, Protocol: protocol, APIKey: "local-test", BaseURL: srv.URL, Model: "fixture"}
			client, err := New(p, 1)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.(StreamClient).Stream(context.Background(), "q", 10, func(string) error { return nil })
			if !errors.Is(err, io.ErrUnexpectedEOF) || result.Text != "partial" {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestProviderDoesNotFollowCredentialRedirect(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := NewOpenAI(config.Provider{Name: "test", APIKey: "secret-test", BaseURL: origin.URL, Model: "m"}, 1)
	_, err := c.Complete(context.Background(), "q", 10)
	if err == nil || called {
		t.Fatalf("redirect forwarded: %v %v", called, err)
	}
}
