package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/model"
)

func TestNativeOpenAIRejectsEventsAfterCompletion(t *testing.T) {
	for _, extra := range []string{
		`{"type":"response.output_text.delta","delta":"late"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"late","name":"write_file","arguments":"{}"}]}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", `{"type":"response.completed","response":{"status":"completed","output_text":"done"}}`, extra)
		}))
		c := NewOpenAI(config.Provider{Name: "test", APIKey: "test", BaseURL: srv.URL, Model: "test"}, 1).(ChatStreamClient)
		out, err := c.ChatStream(context.Background(), ChatRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}, MaxOutputTokens: 64}, func(string) error { return nil })
		srv.Close()
		if err == nil || len(out.ToolCalls) > 0 {
			t.Fatalf("late event accepted: %+v %v", out, err)
		}
	}
}
