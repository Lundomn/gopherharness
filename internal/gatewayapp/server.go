// Package gatewayapp owns the channel application's resources and shutdown order.
package gatewayapp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/feishu"
	"github.com/Lundomn/gopherharness/internal/gateway"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/permission"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/workspace"
)

type Config struct {
	Agent                  runtime.Options
	Feishu                 feishu.Config
	UserSlots, SystemSlots int
	OnError                func(error)
}

type Server struct {
	Handler   http.Handler
	service   *runtime.Service
	gateway   *gateway.Gateway
	journal   *journal.Journal
	adapter   *feishu.Adapter
	socket    *feishu.Socket
	mu        sync.Mutex
	closing   bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// New replays only known-unstarted messages before opening intake. Remote
// capabilities default to read-only, and explicit write tools still need an
// actual approver; channel configuration never silently grants auto approval.
func New(ctx context.Context, c Config) (*Server, error) {
	if c.UserSlots < 1 || c.SystemSlots < 1 {
		return nil, errors.New("foreground and background slots must be positive")
	}
	w, err := workspace.Open(c.Agent.Root)
	if err != nil {
		return nil, err
	}
	c.Agent.Root, c.Feishu.Root = w.Root, w.Root
	if len(c.Agent.AllowedTools) == 0 {
		c.Agent.AllowedTools = []string{"read_file", "list_files", "search"}
	}
	if c.Agent.Approval == "" {
		c.Agent.Approval = permission.Never
	}
	if c.Agent.Approval != permission.Never && (c.Agent.Approval != permission.Ask || c.Agent.Approver == nil) {
		return nil, errors.New("remote tools require never policy or ask with an explicit approver; auto approval is forbidden")
	}
	c.Agent.DisableAutoDream = true
	c.Agent.Resume = ""
	c.Agent.SharedMemory, c.Agent.SharedTodos = nil, nil
	var g *gateway.Gateway
	adapter, err := feishu.New(c.Feishu, func(e gateway.Event) error { return g.Publish(e) })
	if err != nil {
		return nil, err
	}
	j, err := journal.Open(ctx, w.Root)
	if err != nil {
		return nil, err
	}
	s := &Server{journal: j, adapter: adapter, closeDone: make(chan struct{})}
	s.service = runtime.NewService(c.Agent, c.UserSlots, c.SystemSlots)
	hub := delivery.New(100, 3, time.Second, 15*time.Second, nil)
	hub.Journal = j
	// Registration cannot fail for this non-empty, single fresh outlet.
	if err = hub.Register("feishu", adapter); err != nil {
		_ = hub.Close(ctx)
		_ = s.service.Close(ctx)
		j.Close()
		return nil, err
	}
	g = gateway.New(s.service.Scheduler, hub, adapter.Allowed, c.OnError)
	g.Journal, s.gateway = j, g
	s.service.SetWorkerSummarySink(s.postWorkerSummary)
	if err = g.Replay(ctx); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/events/feishu", adapter.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		ready := !s.closing && (s.adapter.Config.Transport != "websocket" || (s.socket != nil && s.socket.Ready()))
		s.mu.Unlock()
		if !ready {
			http.Error(w, "channel not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	s.Handler = mux
	return s, nil
}

// StartSocket is separate from New: New's setup deadline must not cancel the
// service's long-lived channel connection. Only one intake run is owned here.
func (s *Server) StartSocket(ctx context.Context) (*feishu.Socket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.socket != nil {
		return nil, errors.New("gateway socket already started or closing")
	}
	var err error
	s.socket, err = s.adapter.StartSocket(ctx)
	return s.socket, err
}

// Close keeps journal ownership until all accepted work and delivery have
// stopped. A caller deadline triggers cancellation, not premature lock release.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		socket := s.socket
		s.mu.Unlock()
		go func() {
			if socket != nil {
				s.closeErr = socket.Close(ctx)
				// A timed-out waiter does not release the adapter's ownership.
				_ = socket.Close(context.Background())
			}
			s.closeErr = errors.Join(s.closeErr, s.gateway.Close(ctx))
			// If the first wait timed out, cleanup still owns the resources.
			s.closeErr = errors.Join(s.closeErr, s.gateway.Close(context.Background()), s.service.Close(context.Background()))
			s.journal.Close()
			close(s.closeDone)
		}()
	})
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
