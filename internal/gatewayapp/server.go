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
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

// New replays only known-unstarted messages before opening HTTP intake. The
// channel entry is intentionally read-only; local CLI keeps its own permissions.
func New(ctx context.Context, c Config) (*Server, error) {
	if c.UserSlots < 1 || c.SystemSlots < 1 {
		return nil, errors.New("foreground and background slots must be positive")
	}
	w, err := workspace.Open(c.Agent.Root)
	if err != nil {
		return nil, err
	}
	c.Agent.Root, c.Feishu.Root = w.Root, w.Root
	c.Agent.AllowedTools = []string{"read_file", "list_files", "search"}
	c.Agent.Approval = permission.Never
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
	s := &Server{journal: j, closeDone: make(chan struct{})}
	s.service = runtime.NewService(c.Agent, c.UserSlots, c.SystemSlots)
	hub := delivery.New(128, 2, 200*time.Millisecond, 15*time.Second, nil)
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
	s.Handler = mux
	return s, nil
}

// Close keeps journal ownership until all accepted work and delivery have
// stopped. A caller deadline triggers cancellation, not premature lock release.
func (s *Server) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		go func() {
			s.closeErr = s.gateway.Close(ctx)
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
