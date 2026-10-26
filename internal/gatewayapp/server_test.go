package gatewayapp

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/feishu"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
)

const testEncryptKey = "test-encryption-key-not-a-real-credential"

func webhook(t *testing.T, payload, stamp string) *http.Request {
	t.Helper()
	key := sha256.Sum256([]byte(testEncryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(payload)%aes.BlockSize
	plain := append([]byte(payload), bytes.Repeat([]byte{byte(pad)}, pad)...)
	// Fixed IV is only for deterministic test events. Production uses SDK decryption.
	encrypted := make([]byte, aes.BlockSize+len(plain))
	cipher.NewCBCEncrypter(block, encrypted[:aes.BlockSize]).CryptBlocks(encrypted[aes.BlockSize:], plain)
	raw, err := json.Marshal(map[string]string{"encrypt": base64.StdEncoding.EncodeToString(encrypted)})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/events/feishu", bytes.NewReader(raw))
	r.Header.Set("X-Lark-Request-Timestamp", stamp)
	r.Header.Set("X-Lark-Request-Nonce", "test-nonce")
	r.Header.Set("X-Lark-Signature", larkevent.Signature(stamp, "test-nonce", testEncryptKey, string(raw)))
	return r
}

type testApprover bool

func (a testApprover) Approve(string, map[string]any) bool { return bool(a) }

func TestConfiguredRemoteToolsCannotBypassApproval(t *testing.T) {
	for _, policy := range []permission.Policy{permission.Auto, permission.Ask} {
		c := testConfig(t.TempDir(), "", &provider.Fake{})
		c.Agent.Approval = policy
		c.Agent.AllowedTools = []string{"write_file"}
		if s, err := New(context.Background(), c); err == nil {
			closeServer(t, s)
			t.Fatalf("accepted unsafe %s", policy)
		}
	}
	c := testConfig(t.TempDir(), "", &provider.Fake{})
	c.Agent.Approval = permission.Ask
	c.Agent.Approver = testApprover(false)
	c.Agent.AllowedTools = []string{"read_file", "write_file"}
	s, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	closeServer(t, s)
}

func TestReadyEndpointDoesNotClaimUnstartedSocketReady(t *testing.T) {
	c := testConfig(t.TempDir(), "", &provider.Fake{})
	c.Feishu.Transport = "websocket"
	s, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, s)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/events/feishu", 404}} {
		w := httptest.NewRecorder()
		s.Handler.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s got %d", tc.path, w.Code)
		}
	}
}

func eventPayload(sender, chatType string, mention bool) string {
	mentions := `[]`
	if mention {
		mentions = `[{"key":"@_bot","id":{"open_id":"bot"}}]`
	}
	return fmt.Sprintf(`{"schema":"2.0","header":{"event_id":"event-1","event_type":"im.message.receive_v1","app_id":"test-app","token":"test-token"},"event":{"sender":{"sender_type":"user","tenant_key":"tenant","sender_id":{"open_id":%q}},"message":{"message_id":"message-1","chat_id":"chat-1","thread_id":"thread-1","chat_type":%q,"message_type":"text","content":"{\"text\":\"@_bot explain this repo\"}","mentions":%s}}}`, sender, chatType, mentions)
}

func testConfig(root, base string, fake *provider.Fake) Config {
	return Config{Agent: runtime.Options{Root: root, Config: config.Defaults(), Provider: fake, MaxSteps: 4},
		Feishu: feishu.Config{AppID: "test-app", AppSecret: "test-secret", VerificationToken: "test-token", EncryptKey: testEncryptKey, BotOpenID: "bot", AllowedSenders: []string{"allowed"}, BaseURL: base}, UserSlots: 2, SystemSlots: 1}
}

func closeServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestEncryptedWebhookToRuntimeAndDurableDelivery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	sent := make(chan map[string]string, 4)
	var authCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/v3/tenant_access_token/internal":
			authCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"test-access","expire":7200}`))
		case "/im/v1/messages":
			if r.Header.Get("Authorization") != "Bearer test-access" || r.URL.Query().Get("receive_id_type") != "chat_id" {
				t.Error("invalid reply authentication or route")
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			sent <- body
			_, _ = w.Write([]byte(`{"code":0,"data":{"message_id":"reply-1"}}`))
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	fake := &provider.Fake{Responses: []string{
		`<tool>{"name":"write_file","args":{"path":"forbidden.txt","content":"must not write"}}</tool>`,
		`<tool>{"name":"read_file","args":{"path":"README.md"}}</tool>`, `<final>repo explained</final>`}}
	c := testConfig(root, api.URL, fake)
	s, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeServer(t, s) })
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.Handler.ServeHTTP(w, webhook(t, eventPayload("allowed", "group", true), stamp))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	select {
	case body := <-sent:
		if body["receive_id"] != "chat-1" || !strings.Contains(body["content"], "repo explained") || body["uuid"] == "" {
			t.Fatal(body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
	closeServer(t, s)
	if _, err := os.Stat(filepath.Join(root, "forbidden.txt")); !os.IsNotExist(err) {
		t.Fatal("remote write was not denied")
	}
	if authCalls.Load() != 1 || len(sent) != 0 {
		t.Fatal("duplicate intake triggered duplicate delivery")
	}
	j, err := journal.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := j.List(context.Background(), "outbox", "delivered")
	j.Close()
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v error=%v", records, err)
	}
	var persisted delivery.Message
	if err := json.Unmarshal(records[0].Payload, &persisted); err != nil || len(persisted.Identity.TraceID) != 32 {
		t.Fatalf("missing trace identity in durable result: %+v / %v", persisted, err)
	}
	// Restart must not re-execute a completed event, even after in-memory dedup is lost.
	c.Agent.Provider = &provider.Fake{}
	s2, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s2.Handler.ServeHTTP(w, webhook(t, eventPayload("allowed", "group", true), stamp))
	closeServer(t, s2)
	if w.Code != 200 || len(sent) != 0 {
		t.Fatalf("restart duplicate: %d / %d", w.Code, len(sent))
	}
}

func TestWebhookRejectsUnauthenticatedAndUnauthorizedEvents(t *testing.T) {
	s, err := New(context.Background(), testConfig(t.TempDir(), "http://127.0.0.1:1", &provider.Fake{}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer(t, s)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	for _, tc := range []struct {
		name, payload, stamp string
		mutate               func(*http.Request)
		rejected             bool
	}{
		{"bad signature", eventPayload("allowed", "group", true), stamp, func(r *http.Request) { r.Header.Set("X-Lark-Signature", "wrong") }, true},
		{"stale", eventPayload("allowed", "group", true), strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10), nil, true},
		{"missing timestamp", eventPayload("allowed", "group", true), "", nil, true},
		{"bad timestamp", eventPayload("allowed", "group", true), stamp + "oops", nil, true},
		{"not mentioned", eventPayload("allowed", "group", false), stamp, nil, false},
		{"sender denied", eventPayload("denied", "p2p", false), stamp, nil, false},
		{"challenge", `{"type":"url_verification","token":"test-token","challenge":"test-challenge"}`, stamp, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := webhook(t, tc.payload, tc.stamp)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			w := httptest.NewRecorder()
			s.Handler.ServeHTTP(w, r)
			if (w.Code != 200) != tc.rejected {
				t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
			}
			if tc.name == "challenge" && !strings.Contains(w.Body.String(), "test-challenge") {
				t.Fatal(w.Body.String())
			}
		})
	}
	entries, err := s.journal.List(context.Background(), "inbox", "")
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected event reached runtime: %v / %v", entries, err)
	}
	for _, tc := range []struct {
		method, body string
		want         int
	}{
		{http.MethodGet, "", 405}, {http.MethodPost, strings.Repeat("x", (1<<20)+1), 413},
	} {
		w := httptest.NewRecorder()
		s.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, "/events/feishu", strings.NewReader(tc.body)))
		if w.Code != tc.want {
			t.Fatalf("want %d got %d", tc.want, w.Code)
		}
	}
}

func TestGatewayRequiresExplicitAllowlist(t *testing.T) {
	for _, ids := range [][]string{nil, {""}, {"*"}, {"allowed", " "}} {
		c := testConfig(t.TempDir(), "", &provider.Fake{})
		c.Feishu.AllowedSenders = ids
		if s, err := New(context.Background(), c); err == nil {
			closeServer(t, s)
			t.Fatalf("accepted invalid allowlist %v", ids)
		}
	}
}
