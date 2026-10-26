package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Lundomn/gopherharness/internal/config"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/tracing"
)

// Price is explicitly configured per million tokens; no guessed live prices.
type Price struct{ Input, Output, CacheRead, CacheWrite float64 }
type CallRecord struct {
	AttemptID       string           `json:"attempt_id"`
	RequestedModel  string           `json:"requested_model"`
	AttemptedModel  string           `json:"attempted_model"`
	ActualModel     string           `json:"actual_model,omitempty"`
	AccountingModel string           `json:"accounting_model"`
	Identity        tracing.Identity `json:"identity"`
	Usage           Usage            `json:"usage"`
	Outcome         string           `json:"outcome"`
	Cost            *float64         `json:"estimated_cost_usd"`
	ObservedAt      time.Time        `json:"observed_at"`
}
type LedgerHealth struct {
	Accepted  uint64 `json:"accepted"`
	Persisted uint64 `json:"persisted"`
	Lost      uint64 `json:"lost"`
	Closed    bool   `json:"closed"`
}
type Ledger struct {
	mu        sync.Mutex
	health    LedgerHealth
	queue     chan CallRecord
	done      chan struct{}
	file      *os.File
	path      string
	closeOnce sync.Once
	closeErr  error
}

func OpenLedger(path string, capacity int) (*Ledger, error) {
	if capacity < 1 {
		capacity = 256
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	l := &Ledger{file: f, path: path, queue: make(chan CallRecord, capacity), done: make(chan struct{})}
	go l.write()
	return l, nil
}
func (l *Ledger) Append(record CallRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.health.Accepted++
	if l.health.Closed {
		l.health.Lost++
		return
	}
	select {
	case l.queue <- record:
	default:
		l.health.Lost++
	}
}
func (l *Ledger) Health() LedgerHealth { l.mu.Lock(); defer l.mu.Unlock(); return l.health }
func (l *Ledger) write() {
	defer close(l.done)
	for r := range l.queue {
		data, err := json.Marshal(r)
		if err == nil {
			var n int
			n, err = l.file.Write(append(data, '\n'))
			if err == nil && n != len(data)+1 {
				err = errors.New("short ledger write")
			}
		}
		l.mu.Lock()
		if err != nil {
			l.health.Lost++
		} else {
			l.health.Persisted++
		}
		l.mu.Unlock()
	}
}
func (l *Ledger) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.health.Closed = true
		close(l.queue)
		l.mu.Unlock()
		<-l.done
		syncErr := l.file.Sync()
		closeErr := l.file.Close()
		data, err := json.Marshal(l.Health())
		if err == nil {
			err = os.WriteFile(l.path+".health.json", data, 0600)
		}
		l.closeErr = errors.Join(syncErr, closeErr, err)
	})
	return l.closeErr
}

type Metered struct {
	Delegate Client
	Ledger   *Ledger
	prices   map[string]Price
}

func Meter(delegate Client, ledger *Ledger, prices map[string]Price) *Metered {
	copy := map[string]Price{}
	for k, v := range prices {
		copy[k] = v
	}
	return &Metered{Delegate: delegate, Ledger: ledger, prices: copy}
}
func (m *Metered) Name() string  { return m.Delegate.Name() }
func (m *Metered) Model() string { return m.Delegate.Model() }
func (m *Metered) ContextPolicy() (int, int, string) {
	if p, ok := m.Delegate.(ContextProfiler); ok {
		return p.ContextPolicy()
	}
	return (config.Provider{}).ContextPolicy()
}
func (m *Metered) record(ctx context.Context, result Completion, err error, stream bool) {
	outcome := "success"
	switch {
	case errors.Is(err, context.Canceled):
		outcome = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		outcome = "timed_out"
	case err != nil:
		outcome = "error"
	case stream && !result.Usage.Complete:
		outcome = "incomplete"
	}
	accounting := result.ActualModel
	if accounting == "" {
		accounting = m.Model()
	}
	var cost *float64
	if price, ok := m.prices[accounting]; ok && result.Usage.Complete && price.Input >= 0 && price.Output >= 0 && price.CacheRead >= 0 && price.CacheWrite >= 0 {
		value := (float64(result.Usage.InputTokens)*price.Input + float64(result.Usage.OutputTokens)*price.Output + float64(result.Usage.CacheReadTokens)*price.CacheRead + float64(result.Usage.CacheWriteTokens)*price.CacheWrite) / 1e6
		cost = &value
	}
	id := BudgetAttemptID(ctx)
	if id == "" {
		var b [16]byte
		if _, e := rand.Read(b[:]); e == nil {
			id = hex.EncodeToString(b[:])
		}
	}
	m.Ledger.Append(CallRecord{AttemptID: id, RequestedModel: requestedModel(ctx, m.Model()), AttemptedModel: m.Model(), ActualModel: result.ActualModel, AccountingModel: accounting, Identity: tracing.Current(ctx), Usage: result.Usage, Outcome: outcome, Cost: cost, ObservedAt: time.Now().UTC()})
}
func (m *Metered) Complete(ctx context.Context, prompt string, max int) (Completion, error) {
	r, err := m.Delegate.Complete(ctx, prompt, max)
	m.record(ctx, r, err, false)
	return r, err
}
func (m *Metered) Stream(ctx context.Context, prompt string, max int, delta func(string) error) (Completion, error) {
	var r Completion
	var err error
	if stream, ok := m.Delegate.(StreamClient); ok {
		r, err = stream.Stream(ctx, prompt, max, delta)
	} else {
		r, err = m.Delegate.Complete(ctx, prompt, max)
		if err == nil {
			err = delta(r.Text)
		}
	}
	m.record(ctx, r, err, true)
	return r, err
}
func (m *Metered) InspectImage(ctx context.Context, mime string, data []byte, prompt string, max int) (Completion, error) {
	v, ok := m.Delegate.(VisionClient)
	if !ok {
		return Completion{}, errors.New("provider does not support vision")
	}
	r, err := v.InspectImage(ctx, mime, data, prompt, max)
	m.record(ctx, r, err, false)
	return r, err
}
