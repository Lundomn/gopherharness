package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHTTPMemoryScopeDurableReceiptAndIdempotentTurnKey(t *testing.T) {
	var mu sync.Mutex
	keys := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing scoped credential")
		}
		switch r.URL.Path {
		case "/v1/recall":
			var request HTTPRecallRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Repo != "repo-a" || request.User != "user-a" || request.TopK != 5 {
				t.Error(request)
			}
			_ = json.NewEncoder(w).Encode(HTTPRecallResponse{Repo: request.Repo, User: request.User, Memories: []HTTPMemory{{ID: "fact", Text: "use Go", Score: .9}}})
		case "/v1/store":
			var request HTTPStoreRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(HTTPStoreResponse{Repo: request.Repo, User: request.User, TurnID: request.TurnID, Durable: true})
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	b, err := NewHTTPBackend(HTTPOptions{BaseURL: server.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	scope := Scope{Repo: "repo-a", User: "user-a"}
	facts, err := b.Recall(context.Background(), scope, "Go", 5)
	if err != nil || len(facts) != 1 || facts[0].Scope != scope {
		t.Fatalf("%v %v", facts, err)
	}
	for i := 0; i < 2; i++ {
		if err := b.Store(context.Background(), scope, "turn-a", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Store(context.Background(), Scope{Repo: "repo-a", User: "user-b"}, "turn-a", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 3 || keys[0] == "" || keys[0] != keys[1] || keys[0] == keys[2] {
		t.Fatal(keys)
	}
}
func TestHTTPMemoryRejectsCrossScopeAndNonDurableAcceptance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/recall" {
			_ = json.NewEncoder(w).Encode(HTTPRecallResponse{Repo: "other", User: "other"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	b, _ := NewHTTPBackend(HTTPOptions{BaseURL: server.URL})
	defer b.Close()
	if _, err := b.Recall(context.Background(), Scope{"repo", "user"}, "query", 5); err == nil {
		t.Fatal("cross-scope recall accepted")
	}
	if err := b.Store(context.Background(), Scope{"repo", "user"}, "turn", nil); err == nil {
		t.Fatal("accepted async queue as durable")
	}
}
func TestHTTPMemoryTimeoutCancellationAndRedirectAreBounded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	b, _ := NewHTTPBackend(HTTPOptions{BaseURL: server.URL, Timeout: 20 * time.Millisecond})
	defer b.Close()
	if _, err := b.Recall(context.Background(), Scope{"repo", "user"}, "query", 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Store(ctx, Scope{"repo", "user"}, "turn", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, _ := NewHTTPBackend(HTTPOptions{BaseURL: redirect.URL})
	defer c.Close()
	if _, err := c.Recall(context.Background(), Scope{"repo", "user"}, "query", 5); err == nil {
		t.Fatal("redirect followed")
	}
}
