package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

const DefaultConcurrency = 4
const DefaultHourlyLimit = 30
const DefaultInboxCapacity = 50

var ErrQuota = errors.New("worker rolling-hour quota exceeded")

type Runner func(context.Context, string, string, <-chan string) (string, error)
type Worker struct {
	ID, Role, Prompt, Status, Result, Error string
	CreatedAt, StartedAt, FinishedAt        time.Time
	cancel                                  context.CancelFunc
	inbox                                   chan string
	done                                    chan struct{}
	PersistenceError                        string
}
type Options struct {
	// Lifetime belongs to the session/service, not one foreground turn.
	Lifetime                 context.Context
	Concurrency, HourlyLimit int
	Now                      func() time.Time
	Root, Session, Principal string
	Redact                   func(string) string
}
type Manager struct {
	mu                 sync.RWMutex
	workers            map[string]*Worker
	runner             Runner
	seq                int
	closed             bool
	wg                 sync.WaitGroup
	lifetime           context.Context
	cancel             context.CancelFunc
	slots              chan struct{}
	hourlyLimit        int
	admissions         []time.Time
	now                func() time.Time
	results            *durable.File[resultState]
	releaseResults     func()
	closeOnce          sync.Once
	principal, session string
	redact             func(string) string
	notify             func()
	notificationErr    error
}

func New(runner Runner) *Manager { return NewWithOptions(runner, Options{}) }
func NewWithOptions(runner Runner, opts Options) *Manager {
	if opts.Lifetime == nil {
		opts.Lifetime = context.Background()
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.HourlyLimit <= 0 {
		opts.HourlyLimit = DefaultHourlyLimit
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(opts.Lifetime)
	return &Manager{workers: map[string]*Worker{}, runner: runner, lifetime: ctx, cancel: cancel, slots: make(chan struct{}, opts.Concurrency), hourlyLimit: opts.HourlyLimit, now: opts.Now}
}

func (m *Manager) Start(role, prompt string) (string, error) {
	return m.StartContext(context.Background(), role, prompt)
}

// StartContext copies only causal identity from the submitting turn. A normal
// return from that turn must not cancel accepted background work.
func (m *Manager) StartContext(origin context.Context, role, prompt string) (string, error) {
	if err := origin.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("worker prompt is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.lifetime.Err() != nil {
		return "", fmt.Errorf("worker manager is closed")
	}
	if m.runner == nil {
		return "", fmt.Errorf("worker runner is required")
	}
	now := m.now().UTC()
	first := 0
	for first < len(m.admissions) && !m.admissions[first].After(now.Add(-time.Hour)) {
		first++
	}
	m.admissions = append([]time.Time(nil), m.admissions[first:]...)
	if len(m.admissions) >= m.hourlyLimit {
		return "", ErrQuota
	}
	m.admissions = append(m.admissions, now)
	m.seq++
	id := m.newID()
	ctx, cancel := context.WithCancel(tracing.Attach(m.lifetime, tracing.Current(origin)))
	w := &Worker{ID: id, Role: role, Prompt: prompt, Status: "queued", CreatedAt: now, cancel: cancel, inbox: make(chan string, DefaultInboxCapacity), done: make(chan struct{})}
	if err := m.persistQueued(w, tracing.Current(origin)); err != nil {
		cancel()
		m.admissions = m.admissions[:len(m.admissions)-1]
		return "", err
	}
	m.workers[id] = w
	m.wg.Add(1)
	go m.run(ctx, w)
	return id, nil
}

func (m *Manager) run(ctx context.Context, w *Worker) {
	defer m.wg.Done()
	defer w.cancel()
	var result string
	var err error
	acquired := false
	select {
	case m.slots <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	if acquired {
		if err = ctx.Err(); err == nil {
			m.mu.Lock()
			if w.Status != "stopping" {
				w.Status = "running"
			}
			w.StartedAt = m.now().UTC()
			m.mu.Unlock()
			result, err = m.invoke(ctx, w)
		}
		// Capacity remains held until the actual runner and its cleanup return.
		<-m.slots
	}
	m.mu.Lock()
	w.FinishedAt = m.now().UTC()
	if ctx.Err() != nil || w.Status == "stopping" {
		w.Status = "stopped"
		if err != nil {
			w.Error = err.Error()
		}
	} else if err != nil {
		w.Status, w.Error = "failed", err.Error()
	} else {
		w.Status, w.Result = "completed", result
	}
	snapshot := *w
	m.mu.Unlock()
	err = m.persistFinished(snapshot)
	m.mu.Lock()
	if err != nil {
		w.PersistenceError = err.Error()
	}
	close(w.done)
	notify := m.notify
	wake := err == nil && !m.closed && w.Status != "stopped"
	m.mu.Unlock()
	if wake && notify != nil {
		notify()
	}
}

func (m *Manager) invoke(ctx context.Context, w *Worker) (result string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("worker panic: %v", p)
		}
	}()
	return m.runner(ctx, w.Role, w.Prompt, w.inbox)
}

func active(status string) bool {
	return status == "queued" || status == "running" || status == "stopping"
}

func (m *Manager) Send(id, message string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.workers[id]
	if !ok {
		return fmt.Errorf("worker %q not found", id)
	}
	if m.closed || (w.Status != "running" && w.Status != "queued") {
		return fmt.Errorf("worker %q is not running", id)
	}
	select {
	case w.inbox <- message:
		return nil
	default:
		return fmt.Errorf("worker %q inbox is full", id)
	}
}

func (m *Manager) Stop(id string) error { return m.StopContext(context.Background(), id) }

// A wait timeout does not mark the worker stopped or release its capacity.
func (m *Manager) StopContext(ctx context.Context, id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("worker %q not found", id)
	}
	if active(w.Status) {
		w.Status = "stopping"
		w.cancel()
	}
	done := w.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) List() []Worker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Worker, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *Manager) RunningIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ids []string
	for id, w := range m.workers {
		if active(w.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
func (m *Manager) CancelAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		if active(w.Status) {
			w.Status = "stopping"
			w.cancel()
		}
	}
}
func (m *Manager) Seal() {
	m.mu.Lock()
	wasClosed := m.closed
	m.closed = true
	m.cancel()
	for _, w := range m.workers {
		if active(w.Status) {
			w.Status = "stopping"
			w.cancel()
		}
	}
	m.mu.Unlock()
	if !wasClosed {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := m.RevokeNotifications(ctx)
		cancel()
		if err != nil {
			m.mu.Lock()
			m.notificationErr = err
			m.mu.Unlock()
		}
	}
}
func (m *Manager) Close() {
	m.Seal()
	m.wg.Wait()
	m.closeOnce.Do(func() {
		if m.releaseResults != nil {
			m.releaseResults()
		}
	})
}

func (m *Manager) NotificationError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notificationErr
}
