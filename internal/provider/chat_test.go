package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tools"
)

func TestNativeWireToolsAndRoundTrip(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			round := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				raw, _ := json.Marshal(body)
				if strings.Contains(string(raw), "<tool>") {
					t.Error("native request flattened into text protocol")
				}
				if !strings.Contains(string(raw), `"properties":{"path":{"type":"string"}}`) {
					t.Errorf("shorthand schema leaked: %s", raw)
				}
				round++
				if round == 2 && !strings.Contains(string(raw), "call_1") {
					t.Error("lost wire call ID")
				}
				if protocol == "openai" {
					if round == 1 {
						fmt.Fprint(w, `{"status":"completed","model":"actual","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"},{"type":"function_call","name":"read_file","call_id":"call_1","arguments":"{\"path\":\"a.txt\"}"}],"usage":{"input_tokens":5,"output_tokens":3}}`)
					} else {
						if !strings.Contains(string(raw), "opaque") {
							t.Error("reasoning item dropped")
						}
						fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
					}
				} else {
					if round == 1 {
						fmt.Fprint(w, `{"model":"actual","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"a.txt"}}],"usage":{"input_tokens":5,"output_tokens":3}}`)
					} else {
						fmt.Fprint(w, `{"stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`)
					}
				}
			}))
			defer server.Close()
			c, e := New(config.Provider{Name: protocol, Protocol: protocol, Model: "m", APIKey: "test-only", BaseURL: server.URL}, 2)
			if e != nil {
				t.Fatal(e)
			}
			req := ChatRequest{Messages: []model.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "read file"}}, Tools: []tools.Spec{{Name: "read_file", Schema: `{"path":"string"}`}}, MaxOutputTokens: 20}
			out, e := c.(ChatClient).Chat(context.Background(), req)
			if e != nil || len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "call_1" {
				t.Fatalf("%+v %v", out, e)
			}
			req.Messages = append(req.Messages, model.Message{Role: "assistant", ToolCalls: out.ToolCalls, NativeContent: out.NativeContent}, model.Message{Role: "tool", ToolCallID: "call_1", Content: "hello"})
			out, e = c.(ChatClient).Chat(context.Background(), req)
			if e != nil || out.Text != "done" {
				t.Fatalf("%+v %v", out, e)
			}
		})
	}
}
func TestNativeRejectsOrphanAndDuplicateResults(t *testing.T) {
	for _, ms := range [][]model.Message{
		{{Role: "tool", ToolCallID: "missing"}},
		{{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c", Name: "x"}}}, {Role: "user", Content: "interrupt"}},
		{{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c", Name: "x"}}}, {Role: "tool", ToolCallID: "c"}, {Role: "tool", ToolCallID: "c"}},
	} {
		if ValidateConversation(ms) == nil {
			t.Fatalf("accepted invalid exchange: %+v", ms)
		}
	}
}
func TestNativeAnthropicStreamAcceptsOnlyCompleteToolJSON(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "%s\n\n", `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}`)
				fmt.Fprintf(w, "%s\n\n", `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"write_file","input":{}}}`)
				fmt.Fprintf(w, "%s\n\n", `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\",\"content\":\"y\"}"}}`)
				if complete {
					fmt.Fprintf(w, "%s\n\n", `data: {"type":"content_block_stop","index":0}`)
					fmt.Fprintf(w, "%s\n\n", `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`)
					fmt.Fprintf(w, "%s\n\n", `data: {"type":"message_stop"}`)
				}
			}))
			defer s.Close()
			c := NewAnthropic(config.Provider{Name: "a", APIKey: "test", BaseURL: s.URL, Model: "m"}, 2).(ChatStreamClient)
			out, e := c.ChatStream(context.Background(), ChatRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}, MaxOutputTokens: 30}, func(string) error { return nil })
			if complete {
				if e != nil || len(out.ToolCalls) != 1 {
					t.Fatalf("%+v %v", out, e)
				}
			} else if !errors.Is(e, io.ErrUnexpectedEOF) || len(out.ToolCalls) > 0 {
				t.Fatalf("partial call escaped: %+v %v", out, e)
			}
		})
	}
}
func TestOpenAINativeStreamDoesNotTreatEOFAsSuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "%s\n\n", `data: {"type":"response.output_text.delta","delta":"partial"}`)
	}))
	defer s.Close()
	c := NewOpenAI(config.Provider{Name: "o", APIKey: "test", BaseURL: s.URL, Model: "m"}, 2).(ChatStreamClient)
	out, e := c.ChatStream(context.Background(), ChatRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}, MaxOutputTokens: 20}, func(string) error { return nil })
	if !errors.Is(e, io.ErrUnexpectedEOF) || out.Text != "partial" {
		t.Fatalf("%+v %v", out, e)
	}
}
func TestContextOverflowClassification(t *testing.T) {
	for _, e := range []*HTTPError{{Status: 413}, {Status: 400, Detail: `{"code":"context_length_exceeded"}`}, {Status: 400, Detail: "prompt is too long"}} {
		if !ContextOverflow(e) {
			t.Fatal(e)
		}
	}
	if ContextOverflow(&HTTPError{Status: 400, Detail: "invalid tool schema"}) || ContextOverflow(&HTTPError{Status: 401, Detail: "context_length_exceeded"}) {
		t.Fatal("unrelated failure called overflow")
	}
}
