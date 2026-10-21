package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/state"
)

// Service is the multi-conversation assembly point. CLI's direct Ask remains
// supported. One scheduler lane owns one Agent/Session; files remain shared.
type Service struct {
	Scheduler *scheduler.Scheduler
	opts      Options
	mu        sync.Mutex
	agents    map[string]*Agent
	todos     *state.TodoLedger
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func NewService(opts Options, userSlots, systemSlots int) *Service {
	s := &Service{opts: opts, agents: map[string]*Agent{}, closeDone: make(chan struct{})}
	s.Scheduler = scheduler.New(s.run, userSlots, systemSlots, 128)
	return s
}
func (s *Service) run(ctx context.Context, req scheduler.Request, drain func() []scheduler.Request) (string, error) {
	s.mu.Lock()
	a := s.agents[req.Conversation]
	if a == nil {
		opts := s.opts
		opts.SharedMemory = nil
		opts.MemoryScope = req.Conversation
		opts.MemoryUser = req.Source.SenderID
		opts.SharedTodos = s.todos
		// A single explicitly resumed session cannot be reused by all lanes.
		if opts.Resume != "" {
			s.mu.Unlock()
			return "", fmt.Errorf("multi-conversation service requires independent sessions, not shared Resume")
		}
		var err error
		a, err = New(opts)
		if err != nil {
			s.mu.Unlock()
			return "", err
		}
		if s.todos == nil {
			s.todos = a.Todos
		} else {
			a.Todos = s.todos
		}
		s.agents[req.Conversation] = a
	}
	s.mu.Unlock()
	a.DrainInjected = func() []string {
		var texts []string
		for _, r := range drain() {
			texts = append(texts, r.Text)
		}
		return texts
	}
	defer func() { a.DrainInjected = nil }()
	return a.Ask(ctx, req.Text)
}
func (s *Service) Close(ctx context.Context) error {
	s.Scheduler.Seal()
	s.closeOnce.Do(func() {
		go func() {
			// Admission is sealed and accepted work is cancelled before connections
			// are released. A timed-out caller does not abandon resource ownership.
			s.closeErr = s.Scheduler.Close(context.Background())
			s.mu.Lock()
			agents := make([]*Agent, 0, len(s.agents))
			for _, a := range s.agents {
				agents = append(agents, a)
			}
			s.mu.Unlock()
			for _, a := range agents {
				s.closeErr = errors.Join(s.closeErr, a.Close())
			}
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
