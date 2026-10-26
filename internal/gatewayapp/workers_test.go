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
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

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
