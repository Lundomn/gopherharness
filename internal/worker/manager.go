package worker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Runner func(context.Context, string, string, <-chan string) (string, error)
type Worker struct {
	ID, Role, Prompt, Status, Result, Error string
	StartedAt, FinishedAt                   time.Time
	cancel                                  context.CancelFunc
	inbox                                   chan string
}
type Manager struct {
	mu      sync.RWMutex
	workers map[string]*Worker
	runner  Runner
	seq     int
	closed  bool
	wg      sync.WaitGroup
}

func New(runner Runner) *Manager { return &Manager{workers: map[string]*Worker{}, runner: runner} }
func (m *Manager) Start(role, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("worker prompt is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("worker manager is closed")
	}
	m.seq++
	id := fmt.Sprintf("worker-%03d", m.seq)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{ID: id, Role: role, Prompt: prompt, Status: "running", StartedAt: time.Now().UTC(), cancel: cancel, inbox: make(chan string, 8)}
	m.workers[id] = w
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		result, err := m.runner(ctx, role, prompt, w.inbox)
		m.mu.Lock()
		defer m.mu.Unlock()
		if w.Status == "stopped" {
			return
		}
		w.FinishedAt = time.Now().UTC()
		if err != nil {
			w.Status = "failed"
			w.Error = err.Error()
		} else {
			w.Status = "completed"
			w.Result = result
		}
	}()
	return id, nil
}
func (m *Manager) Send(id, message string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.workers[id]
	if !ok {
		return fmt.Errorf("worker %q not found", id)
	}
	if w.Status != "running" {
		return fmt.Errorf("worker %q is not running", id)
	}
	select {
	case w.inbox <- message:
		return nil
	default:
		return fmt.Errorf("worker %q inbox is full", id)
	}
}
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("worker %q not found", id)
	}
	if w.Status != "running" {
		m.mu.Unlock()
		return nil
	}
	w.Status = "stopped"
	w.FinishedAt = time.Now().UTC()
	m.mu.Unlock()
	w.cancel()
	return nil
}
func (m *Manager) List() []Worker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Worker, 0, len(m.workers))
	for _, w := range m.workers {
		out = append(out, *w)
	}
	return out
}

func (m *Manager) RunningIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ids []string
	for id, worker := range m.workers {
		if worker.Status == "running" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return
	}
	m.closed = true
	for _, worker := range m.workers {
		if worker.Status == "running" {
			worker.Status = "stopped"
			worker.FinishedAt = time.Now().UTC()
			worker.cancel()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}
