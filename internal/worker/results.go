package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type Receipt struct {
	ID, Role, Prompt, Status, Result, Error string
	CreatedAt, StartedAt, FinishedAt        time.Time
	Trace                                   tracing.Identity
	Acknowledged, Revoked                   bool
}
type resultState struct {
	Session, Principal string
	Records            map[string]Receipt
	Admissions         []time.Time
}

// NewDurable holds an OS lease for the manager lifetime. Only after acquiring
// it can old unfinished workers be classified as interrupted; another live
// process is never mistaken for a crashed owner or restarted automatically.
func NewDurable(runner Runner, opts Options) (*Manager, error) {
	if opts.Root == "" || opts.Session == "" || opts.Principal == "" {
		return nil, errors.New("durable worker manager requires root, session and principal")
	}
	h := sha256.Sum256([]byte(opts.Session))
	path := filepath.Join(opts.Root, ".pico", "workers", hex.EncodeToString(h[:])+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release, err := durable.Lease(ctx, path+".owner.lock")
	if err != nil {
		return nil, fmt.Errorf("worker session is owned by another live runtime: %w", err)
	}
	m := NewWithOptions(runner, opts)
	m.results = &durable.File[resultState]{Path: path}
	m.releaseResults = release
	m.principal = opts.Principal
	m.session = opts.Session
	m.redact = opts.Redact
	if m.redact == nil {
		m.redact = func(s string) string { return s }
	}
	err = m.results.Update(context.Background(), func(state *resultState) error {
		if state.Session != "" && (state.Session != opts.Session || state.Principal != opts.Principal) {
			return errors.New("worker result ownership mismatch")
		}
		state.Session, state.Principal = opts.Session, opts.Principal
		if state.Records == nil {
			state.Records = map[string]Receipt{}
		}
		for id, r := range state.Records {
			if active(r.Status) {
				r.Status = "interrupted"
				r.Error = "owning runtime exited before a terminal result; worker was not restarted"
				r.FinishedAt = m.now().UTC()
				state.Records[id] = r
			}
			w := &Worker{ID: r.ID, Role: r.Role, Prompt: r.Prompt, Status: r.Status, Result: r.Result, Error: r.Error, CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, done: make(chan struct{}), cancel: func() {}}
			close(w.done)
			m.workers[id] = w
		}
		m.admissions = append([]time.Time(nil), state.Admissions...)
		return nil
	})
	if err != nil {
		m.cancel()
		release()
		return nil, err
	}
	return m, nil
}
func (m *Manager) persistQueued(w *Worker, trace tracing.Identity) error {
	if m.results == nil {
		return nil
	}
	return m.results.Update(context.Background(), func(s *resultState) error {
		s.Records[w.ID] = Receipt{ID: w.ID, Role: m.redact(w.Role), Prompt: m.redact(w.Prompt), Status: "queued", CreatedAt: w.CreatedAt, Trace: trace}
		s.Admissions = append([]time.Time(nil), m.admissions...)
		return nil
	})
}
func (m *Manager) persistFinished(w Worker) error {
	if m.results == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.results.Update(ctx, func(s *resultState) error {
		r, ok := s.Records[w.ID]
		if !ok {
			return errors.New("worker admission receipt missing")
		}
		r.Status, r.Result, r.Error = w.Status, m.redact(w.Result), m.redact(w.Error)
		r.StartedAt, r.FinishedAt = w.StartedAt, w.FinishedAt
		if w.Status == "stopped" {
			r.Revoked = true
		}
		s.Records[w.ID] = r
		return nil
	})
}
func (m *Manager) PendingResults(ctx context.Context) ([]Receipt, error) {
	if m.results == nil {
		return nil, nil
	}
	s, err := m.results.Read(ctx)
	if err != nil {
		return nil, err
	}
	var out []Receipt
	for _, r := range s.Records {
		if !active(r.Status) && !r.Acknowledged && !r.Revoked {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (m *Manager) Acknowledge(ctx context.Context, id string) error {
	if m.results == nil {
		return nil
	}
	return m.results.Update(ctx, func(s *resultState) error {
		r, ok := s.Records[id]
		if !ok {
			return errors.New("unknown worker receipt")
		}
		if active(r.Status) {
			return errors.New("worker not finished")
		}
		r.Acknowledged = true
		s.Records[id] = r
		return nil
	})
}
func (m *Manager) RevokeNotifications(ctx context.Context) error {
	if m.results == nil {
		return nil
	}
	return m.results.Update(ctx, func(s *resultState) error {
		for id, r := range s.Records {
			r.Revoked = true
			s.Records[id] = r
		}
		return nil
	})
}
func (m *Manager) SetNotifier(notify func()) {
	m.mu.Lock()
	m.notify = notify
	m.mu.Unlock()
}
func (m *Manager) newID() string {
	if m.results != nil {
		return store.NewID("worker")
	}
	return fmt.Sprintf("worker-%03d", m.seq)
}
