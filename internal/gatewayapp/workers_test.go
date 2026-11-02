package gatewayapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

func TestPendingReplyReplayRechecksCurrentAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source scheduler.Source
		want   string
		sends  int32
	}{
		{"removed sender", scheduler.Source{Channel: "feishu", ChatID: "trusted-chat", SenderID: "removed"}, "dropped", 0},
		{"removed chat", scheduler.Source{Channel: "feishu", ChatID: "removed-chat", SenderID: "allowed"}, "dropped", 0},
		{"still allowed", scheduler.Source{Channel: "feishu", ChatID: "trusted-chat", SenderID: "allowed"}, "delivered", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			j, err := journal.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			// A result accepted before shutdown survives a later allowlist change.
			_, err = j.Put(ctx, "outbox", "worker:saved-result", delivery.Message{
				ID: "worker:saved-result", Source: tc.source, Text: "already computed",
			})
			j.Close()
			if err != nil {
				t.Fatal(err)
			}
			var sends, requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/auth/v3/tenant_access_token/internal":
					_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"test-access","expire":7200}`))
				case "/im/v1/messages":
					sends.Add(1)
					_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"saved-reply"}}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			c := testConfig(root, api.URL, &provider.Fake{})
			c.Feishu.AllowedChats = []string{"trusted-chat"}
			for generation := 0; generation < 2; generation++ {
				s, err := New(ctx, c)
				if err != nil {
					t.Fatal(err)
				}
				defer closeServer(t, s)
				if err := s.gateway.Hub.WaitIdle(ctx); err != nil {
					t.Fatal(err)
				}
				e, ok, err := s.journal.Get(ctx, "outbox", "worker:saved-result")
				if err != nil || !ok || e.State != tc.want || sends.Load() != tc.sends {
					t.Errorf("generation %d: entry=%+v err=%v sends=%d, want %s/%d", generation, e, err, sends.Load(), tc.want, tc.sends)
				}
				closeServer(t, s)
			}
			if tc.sends == 0 && requests.Load() != 0 {
				t.Errorf("revoked reply reached remote API: requests=%d", requests.Load())
			}
		})
	}
}

func TestWorkerSummaryUsesDurableRunIdentityAndTrustedRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sends atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/v3/tenant_access_token/internal":
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"test-access","expire":7200}`))
		case "/im/v1/messages":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["receive_id"] != "trusted-chat" || body["uuid"] == "" {
				t.Error("lost worker reply route or stable id", body)
			}
			sends.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"worker-reply"}}`))
		default:
			t.Error("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	s, err := New(ctx, testConfig(t.TempDir(), api.URL, &provider.Fake{}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, s)
	summary := runtime.WorkerSummary{Conversation: "conversation", SessionID: "session", RunID: "stable-run", Text: "worker result", Source: scheduler.Source{Channel: "feishu", ChatID: "trusted-chat", SenderID: "allowed"}, Identity: tracing.Identity{TraceID: "0123456789abcdef0123456789abcdef"}}
	if err := s.postWorkerSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	if err := s.gateway.Hub.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.postWorkerSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := s.journal.Get(ctx, "outbox", "worker:stable-run")
	var saved delivery.Message
	if err != nil || !ok || json.Unmarshal(entry.Payload, &saved) != nil || entry.State != "delivered" || saved.Identity != summary.Identity || sends.Load() != 1 {
		t.Fatalf("worker summary not durably routed once: %+v %+v %v sends=%d", entry, saved, err, sends.Load())
	}
	summary.Source.SenderID = "denied"
	if err := s.postWorkerSummary(ctx, summary); err == nil {
		t.Fatal("untrusted worker owner accepted")
	}
}
