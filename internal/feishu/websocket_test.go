package feishu

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
	"time"

	"github.com/Lundomn/gopherharness/internal/gateway"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/gorilla/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func TestOfficialWebsocketReceivesEventAndJoinsOnClose(t *testing.T) {
	message := `{"schema":"2.0","header":{"event_id":"evt","event_type":"im.message.receive_v1","app_id":"app"},"event":{"sender":{"sender_type":"user","tenant_key":"tenant","sender_id":{"open_id":"allowed"}},"message":{"message_id":"msg","chat_id":"chat","thread_id":"thread","chat_type":"group","message_type":"text","content":"{\"text\":\"@_bot explain\"}","mentions":[{"key":"@_bot","id":{"open_id":"bot"}}]}}}`
	closed := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == larkws.GenEndpointUri {
			var req larkws.BootstrapRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AppID != "app" || req.AppSecret != "secret" {
				t.Errorf("bootstrap identity: %+v / %v", req, err)
			}
			_ = json.NewEncoder(w).Encode(larkws.EndpointResp{Data: &larkws.Endpoint{Url: "ws" + strings.TrimPrefix(server.URL, "http") + "/socket?device_id=d&service_id=1", ClientConfig: &larkws.ClientConfig{PingInterval: 60}}})
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		defer close(closed)
		frame := larkws.Frame{Method: int32(larkws.FrameTypeData), Headers: []larkws.Header{{Key: larkws.HeaderType, Value: "event"}, {Key: larkws.HeaderSum, Value: "1"}, {Key: larkws.HeaderSeq, Value: "0"}, {Key: larkws.HeaderMessageID, Value: "frame"}}, Payload: []byte(message)}
		raw, err := frame.Marshal()
		if err != nil {
			t.Error(err)
			return
		}
		var roundtrip larkws.Frame
		if err := roundtrip.Unmarshal(raw); err != nil || string(roundtrip.Payload) != message {
			t.Errorf("invalid encoded fixture: %v payload length %d", err, len(roundtrip.Payload))
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
			t.Error(err)
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	entered, release := make(chan gateway.Event, 1), make(chan struct{})
	a, err := New(Config{AppID: "app", AppSecret: "secret", BotOpenID: "bot", AllowedSenders: []string{"allowed"}, AllowedChats: []string{"chat"}, Transport: "websocket"}, func(e gateway.Event) error { entered <- e; <-release; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := a.StartSocket(ctx, larkws.WithDomain(server.URL), larkws.WithAutoReconnect(false))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-entered:
		if e.Request.Text != "explain" || e.Request.Source.ChatID != "chat" || e.Request.Conversation != "tenant:chat:thread:allowed" {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("SDK failed to dispatch local event")
	}
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := s.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("released still-active event handler: %v", err)
	}
	close(release)
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("socket not closed")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWebsocketCancellationDuringBootstrap(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	a, err := New(Config{AppID: "app", AppSecret: "secret", BotOpenID: "bot", AllowedSenders: []string{"sender"}, Transport: "websocket"}, func(gateway.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.StartSocket(context.Background(), larkws.WithDomain(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("bootstrap did not start")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTransportAndChatAllowlistBoundaries(t *testing.T) {
	base := Config{AppID: "app", AppSecret: "secret", BotOpenID: "bot", AllowedSenders: []string{"sender"}, AllowedChats: []string{"only-chat"}, Transport: "websocket"}
	a, err := New(base, func(gateway.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if a.Allowed(scheduler.Source{Channel: "feishu", ChatID: "other", SenderID: "sender"}) {
		t.Fatal("chat allowlist bypass")
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/events/feishu", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	for _, transport := range []string{"invalid", "webhook"} {
		c := base
		c.Transport = transport
		if _, err := New(c, func(gateway.Event) error { return nil }); err == nil {
			t.Fatal(fmt.Sprintf("accepted incomplete %s config", transport))
		}
	}
}
