package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

// WriteJob is recorded BEFORE the main result is committed. Workers require a
// matching completed run report before deriving memory, so a crash on either
// side of that boundary neither loses accepted work nor remembers a failed run.
type WriteJob struct {
	Turn, Session string
	Evidence      []Evidence
	Trace         tracing.Identity
	Attempts      int
	Done          bool
	Failed        bool
	LastError     string
	NextAttempt   time.Time
}
type WriteState struct{ Jobs map[string]WriteJob }
type WriterOptions struct {
	PollInterval, RetryBase, AttemptTimeout time.Duration
	MaxAttempts                             int
}
type Writer struct {
	file    durable.File[WriteState]
	root    string
	scope   Scope
	backend Backend
	opts    WriterOptions
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	closed  bool
	lastErr error
}

func NewWriter(root string, scope Scope, backend Backend, opts WriterOptions) (*Writer, error) {
	if scope.Repo == "" || scope.User == "" || backend == nil {
		return nil, errors.New("memory writer requires explicit scope and backend")
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = time.Second
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = 30 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	key, _ := json.Marshal(scope)
	hash := sha256.Sum256(key)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{file: durable.File[WriteState]{Path: filepath.Join(root, ".pico", "memory", "outbox", hex.EncodeToString(hash[:])+".json")}, root: root, scope: scope, backend: backend, opts: opts, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	// Surface corrupt state at startup instead of silently losing pending work.
	if _, err := w.file.Read(ctx); err != nil {
		cancel()
		return nil, err
	}
	go w.run()
	return w, nil
}

// Prepare makes intent durable, but does not authorize extraction. Notify may
// be called after report persistence; polling also recovers a lost notification.
func (w *Writer) Prepare(ctx context.Context, turn, session string, evidence []Evidence) error {
	if turn == "" || filepath.Base(turn) != turn || session == "" {
		return errors.New("invalid memory turn/session")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("memory writer closed")
	}
	job := WriteJob{Turn: turn, Session: session, Evidence: append([]Evidence(nil), evidence...), Trace: tracing.Current(ctx)}
	for i := range job.Evidence {
		job.Evidence[i].Text = sanitizeDream(job.Evidence[i].Text)
	}
	return w.file.Update(ctx, func(s *WriteState) error {
		if s.Jobs == nil {
			s.Jobs = map[string]WriteJob{}
		}
		if old, ok := s.Jobs[turn]; ok {
			if old.Session != session {
				return errors.New("memory turn belongs to another session")
			}
			return nil
		}
		s.Jobs[turn] = job
		return nil
	})
}
func (w *Writer) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.flush(); err != nil && w.ctx.Err() == nil {
			w.mu.Lock()
			w.lastErr = err
			w.mu.Unlock()
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		case <-w.wake:
		}
	}
}

func (w *Writer) flush() error {
	// This lease spans remote calls, but no state transaction does. Other
	// producers can still append. A process death releases ownership naturally.
	release, err := durable.Lease(w.ctx, w.file.Path+".worker")
	if err != nil {
		return err
	}
	defer release()
	state, err := w.file.Read(w.ctx)
	if err != nil {
		return err
	}
	for key, job := range state.Jobs {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if job.Done || job.Failed || time.Now().Before(job.NextAttempt) {
			continue
		}
		ready, err := w.committed(job)
		if err != nil {
			if recordErr := w.recordResult(key, err); recordErr != nil {
				return recordErr
			}
			continue
		}
		if !ready {
			continue
		}
		ctx, cancel := context.WithTimeout(tracing.Attach(w.ctx, job.Trace), w.opts.AttemptTimeout)
		storeErr := w.backend.Store(ctx, w.scope, job.Turn, job.Evidence)
		cancel()
		// A shutdown cancellation leaves durable work pending for the next owner.
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		err = w.recordResult(key, storeErr)
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) recordResult(key string, storeErr error) error {
	return w.file.Update(w.ctx, func(s *WriteState) error {
		current, ok := s.Jobs[key]
		if !ok || current.Done {
			return nil
		}
		current.Attempts++
		if storeErr == nil {
			current.Done = true
			current.LastError = ""
		} else {
			current.LastError = storeErr.Error()
			current.Failed = current.Attempts >= w.opts.MaxAttempts
			delay := w.opts.RetryBase * time.Duration(1<<min(current.Attempts-1, 8))
			current.NextAttempt = time.Now().Add(min(delay, 5*time.Minute))
		}
		s.Jobs[key] = current
		return nil
	})
}

// Retry explicitly reopens an exhausted job. The five-attempt default prevents
// permanently invalid model output or credentials from causing endless calls.
func (w *Writer) Retry(ctx context.Context, turn string) error {
	err := w.file.Update(ctx, func(s *WriteState) error {
		job, ok := s.Jobs[turn]
		if !ok {
			return errors.New("memory write job not found")
		}
		if job.Done {
			return nil
		}
		job.Failed = false
		job.Attempts = 0
		job.NextAttempt = time.Time{}
		s.Jobs[turn] = job
		return nil
	})
	if err == nil {
		w.Notify()
	}
	return err
}

func (w *Writer) committed(job WriteJob) (bool, error) {
	data, err := os.ReadFile(filepath.Join(w.root, ".pico", "runs", job.Turn, "report.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var report model.RunReport
	if err := json.Unmarshal(data, &report); err != nil {
		return false, fmt.Errorf("memory commit report: %w", err)
	}
	if report.RunID != job.Turn || report.SessionID != job.Session {
		return false, errors.New("memory commit identity mismatch")
	}
	return report.Status == model.StatusCompleted, nil
}

// Pending is useful for diagnostics and local tests. A successful main answer
// does not wait for this count to reach zero.
func (w *Writer) Pending(ctx context.Context) (int, error) {
	state, err := w.file.Read(ctx)
	n := 0
	for _, job := range state.Jobs {
		if !job.Done {
			n++
		}
	}
	return n, err
}
func (w *Writer) LastError() error { w.mu.Lock(); defer w.mu.Unlock(); return w.lastErr }
func (w *Writer) Close() error {
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	<-w.done
	return w.LastError()
}
