package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/gateway"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func adapterForTest(t *testing.T, base string, publish func(gateway.Event) error) *Adapter {
	t.Helper()
	a, err := New(Config{Root: t.TempDir(), AppID: "test-app", AppSecret: "test-secret", VerificationToken: "test-token", EncryptKey: "test-encryption", BotOpenID: "bot", AllowedSenders: []string{"allowed"}, BaseURL: base}, publish)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMediaDownloadIsBoundedAndScopedToWorkspace(t *testing.T) {
	var size atomic.Int64
	size.Store(10)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-access","expire":7200}`)
			return
		}
		if r.URL.Path != "/im/v1/messages/media-1/resources/file-1" || r.URL.Query().Get("type") != "file" {
			t.Errorf("bad resource route: %s", r.URL)
		}
		_, _ = w.Write([]byte(strings.Repeat("a", int(size.Load()))))
	}))
	defer api.Close()
	var published gateway.Event
	a := adapterForTest(t, api.URL, func(e gateway.Event) error { published = e; return nil })
	var e im.P2MessageReceiveV1
	if err := json.Unmarshal([]byte(`{"event":{"sender":{"sender_type":"user","tenant_key":"tenant","sender_id":{"open_id":"allowed"}},"message":{"message_id":"media-1","chat_id":"chat","chat_type":"p2p","message_type":"file","content":"{\"file_key\":\"file-1\"}"}}}`), &e); err != nil {
		t.Fatal(err)
	}
	if err := a.Receive(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(a.Config.Root, ".pico", "media", "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	rel, err := filepath.Rel(a.Config.Root, files[0])
	if err != nil || !strings.Contains(published.Request.Text, rel) {
		t.Fatal(published)
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("attachment permissions: %v / %v", info, err)
	}
	size.Store((10 << 20) + 1)
	if _, err := a.download(context.Background(), "media-1", "file-1", "file"); err == nil {
		t.Fatal("accepted oversized media")
	}
	files, _ = filepath.Glob(filepath.Join(a.Config.Root, ".pico", "media", "*"))
	if len(files) != 1 {
		t.Fatal("oversized download created an artifact")
	}
}

func TestMediaRequestHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-access","expire":7200}`)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	defer api.Close()
	a := adapterForTest(t, api.URL, func(gateway.Event) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.download(ctx, "message", "file", "file"); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("download did not cancel")
	}
}

func TestDeliveryUsesStableIDAndClassifiesHTTPFailures(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var authCalls atomic.Int32
	var uuids []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			authCalls.Add(1)
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"test-access","expire":7200}`)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		uuids = append(uuids, body["uuid"])
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"code":0,"data":{"message_id":"reply"}}`)
	}))
	defer api.Close()
	a := adapterForTest(t, api.URL, func(gateway.Event) error { return nil })
	m := delivery.Message{ID: "stable-id", Source: scheduler.Source{ChatID: "chat"}, Text: "hello"}
	for i := 0; i < 2; i++ {
		if _, err := a.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if authCalls.Load() != 1 || len(uuids) != 2 || uuids[0] == "" || uuids[0] != uuids[1] {
		t.Fatalf("unstable id or token cache: %v", uuids)
	}
	for _, code := range []int32{403, 429, 500} {
		status.Store(code)
		_, err := a.Send(context.Background(), m)
		var terminal delivery.TerminalError
		if err == nil || errors.As(err, &terminal) != (code == 403) {
			t.Fatalf("status %d err=%v", code, err)
		}
	}
}

func TestPostTextIsDeterministic(t *testing.T) {
	post := map[string]any{"z": map[string]any{"text": "last"}, "a": []any{map[string]any{"text": "first"}}, "title": "title"}
	for i := 0; i < 100; i++ {
		if got := postText(post); got != "title\nfirst\nlast\n" {
			t.Fatal(got)
		}
	}
}
