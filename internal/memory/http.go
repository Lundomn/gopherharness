package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// This is the explicit GopherHarness Memory JSON v1 adapter. The readable
// reference defines a plugin contract, not a public HTTP wire format; this is
// not claimed to be a drop-in Mem0/EverMem/Letta client.
type HTTPOptions struct {
	BaseURL, APIKey string
	Timeout         time.Duration
	Client          *http.Client
}
type HTTPMemory struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}
type HTTPMessage struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	ObservedAt time.Time `json:"observed_at"`
}
type HTTPRecallRequest struct {
	Repo  string `json:"repo"`
	User  string `json:"user"`
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}
type HTTPRecallResponse struct {
	Repo     string       `json:"repo"`
	User     string       `json:"user"`
	Memories []HTTPMemory `json:"memories"`
}
type HTTPStoreRequest struct {
	Repo     string        `json:"repo"`
	User     string        `json:"user"`
	TurnID   string        `json:"turn_id"`
	Messages []HTTPMessage `json:"messages"`
}
type HTTPStoreResponse struct {
	Repo    string `json:"repo"`
	User    string `json:"user"`
	TurnID  string `json:"turn_id"`
	Durable bool   `json:"durable"`
}
type HTTPBackend struct {
	base, key      string
	timeout        time.Duration
	client         *http.Client
	ownedTransport *http.Transport
	ctx            context.Context
	cancel         context.CancelFunc
	closed         atomic.Bool
}

func NewHTTPBackend(opts HTTPOptions) (*HTTPBackend, error) {
	u, err := url.Parse(opts.BaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid memory backend URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("memory backend requires HTTPS except loopback development servers")
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &HTTPBackend{base: strings.TrimRight(u.String(), "/"), key: opts.APIKey, timeout: opts.Timeout, ctx: ctx, cancel: cancel}
	if opts.Client != nil {
		copy := *opts.Client
		b.client = &copy
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		b.ownedTransport = transport
		b.client = &http.Client{Transport: transport}
	}
	// Never forward credentials or memory content through a redirect.
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return b, nil
}
func (b *HTTPBackend) Recall(ctx context.Context, scope Scope, query string, limit int) ([]Fact, error) {
	if scope.Repo == "" || scope.User == "" {
		return nil, errors.New("explicit memory scope required")
	}
	if limit <= 0 {
		limit = 5
	}
	limit = min(limit, 50)
	var response HTTPRecallResponse
	if err := b.call(ctx, "/v1/recall", HTTPRecallRequest{Repo: scope.Repo, User: scope.User, Query: query, TopK: limit}, "", &response); err != nil {
		return nil, err
	}
	if response.Repo != scope.Repo || response.User != scope.User {
		return nil, errors.New("memory recall scope mismatch")
	}
	var facts []Fact
	seen := map[string]bool{}
	for _, item := range response.Memories {
		if item.ID == "" || math.IsNaN(item.Score) || math.IsInf(item.Score, 0) || item.Score < 0 || item.Score > 1 {
			return nil, errors.New("invalid memory hit")
		}
		if seen[item.ID] || strings.TrimSpace(item.Text) == "" {
			continue
		}
		seen[item.ID] = true
		facts = append(facts, Fact{ID: item.ID, Subject: "memory", Attribute: "note", Value: sanitizeDream(item.Text), Status: "active", Scope: scope})
		if len(facts) == limit {
			break
		}
	}
	return facts, nil
}
func (b *HTTPBackend) Store(ctx context.Context, scope Scope, turn string, evidence []Evidence) error {
	if scope.Repo == "" || scope.User == "" || turn == "" {
		return errors.New("explicit memory scope and turn required")
	}
	request := HTTPStoreRequest{Repo: scope.Repo, User: scope.User, TurnID: turn, Messages: []HTTPMessage{}}
	for _, item := range evidence {
		request.Messages = append(request.Messages, HTTPMessage{ID: item.ID, Kind: item.Kind, Content: sanitizeDream(item.Text), ObservedAt: item.ObservedAt})
	}
	identity, _ := json.Marshal([]string{scope.Repo, scope.User, turn})
	hash := sha256.Sum256(identity)
	var response HTTPStoreResponse
	if err := b.call(ctx, "/v1/store", request, hex.EncodeToString(hash[:]), &response); err != nil {
		return err
	}
	if response.Repo != scope.Repo || response.User != scope.User || response.TurnID != turn {
		return errors.New("memory store receipt identity mismatch")
	}
	if !response.Durable {
		return errors.New("memory backend accepted data without durable receipt")
	}
	return nil
}
func (b *HTTPBackend) call(parent context.Context, path string, payload any, key string, result any) error {
	if b.closed.Load() {
		return errors.New("memory backend closed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, b.timeout)
	defer cancel()
	stop := context.AfterFunc(b.ctx, cancel)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-GopherHarness-Memory-Version", "1")
	if b.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.key)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("memory transport: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.StatusCode == http.StatusAccepted {
		return fmt.Errorf("memory backend HTTP %d", response.StatusCode)
	}
	const maxBody = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return errors.New("memory response exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("invalid memory response: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing data in memory response")
	}
	return nil
}
func (b *HTTPBackend) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		b.cancel()
		if b.ownedTransport != nil {
			b.ownedTransport.CloseIdleConnections()
		}
	}
	return nil
}
