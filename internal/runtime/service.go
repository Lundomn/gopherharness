package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/state"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

// Service is the multi-conversation assembly point. CLI's direct Ask remains
// supported. One scheduler lane owns one Agent/Session; files remain shared.
type Service struct {
	Scheduler           *scheduler.Scheduler
	opts                Options
	mu                  sync.Mutex
	agents              map[string]*Agent
	owners              map[string]scheduler.Source
	todos               *state.TodoLedger
	closeOnce           sync.Once
	closeDone           chan struct{}
	closeErr            error
	workerCtx           context.Context
	workerCancel        context.CancelFunc
	workerWake          chan string
	summaryWake         chan struct{}
	workerWG            sync.WaitGroup
	workerNotifications map[string]*workerNotification
	workerClosed        bool
	workerSink          func(context.Context, WorkerSummary) error
}

func NewService(opts Options, userSlots, systemSlots int) *Service {
	opts.DisablePersonalSkills = true
	s := &Service{opts: opts, agents: map[string]*Agent{}, owners: map[string]scheduler.Source{}, closeDone: make(chan struct{})}
	s.Scheduler = scheduler.New(s.run, userSlots, systemSlots, 128)
	s.workerCtx, s.workerCancel = context.WithCancel(context.Background())
	s.workerWake = make(chan string, 128)
	s.summaryWake = make(chan struct{}, 1)
	s.workerNotifications = map[string]*workerNotification{}
	s.workerWG.Add(1)
	go s.workerNotificationLoop()
	return s
}
func (s *Service) run(ctx context.Context, req scheduler.Request, drain func() []scheduler.Request) (string, error) {
	s.mu.Lock()
	a := s.agents[req.Conversation]
	owner, owned := s.owners[req.Conversation]
	sharedTodos := s.todos
	s.mu.Unlock()
	if owned && owner != req.Source {
		return "", errors.New("conversation is bound to another source identity")
	}
	if a == nil {
		opts := s.opts
		opts.SharedMemory = nil
		opts.MemoryScope = req.Conversation
		opts.MemoryUser = req.Source.SenderID
		opts.SharedTodos = sharedTodos
		// A single explicitly resumed session cannot be reused by all lanes.
		if opts.Resume != "" {
			return "", fmt.Errorf("multi-conversation service requires independent sessions, not shared Resume")
		}
		var err error
		opts.Resume, err = s.resolveSession(ctx, req)
		if err != nil {
			return "", err
		}
		a, err = New(opts)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		if s.todos == nil {
			s.todos = a.Todos
		} else {
			a.Todos = s.todos
		}
		s.agents[req.Conversation] = a
		s.owners[req.Conversation] = req.Source
		s.mu.Unlock()
		a.Workers.SetNotifier(func() { s.wakeWorker(req.Conversation) })
		s.wakeWorker(req.Conversation)
	}
	a.DrainInjected = func() []string {
		var texts []string
		for _, r := range drain() {
			texts = append(texts, r.Text)
		}
		return texts
	}
	defer func() { a.DrainInjected = nil }()
	if req.WorkerResults {
		text, err := a.summarizeWorkerResults(context.WithValue(ctx, workerSummaryRunKey{}, req.WorkerSummaryRunID))
		if err == nil && text != "" {
			r, found, loadErr := nextSummary(ctx, a.Workspace.Root, req.Conversation)
			if loadErr != nil {
				return "", loadErr
			}
			if !found || r.Summary.RunID != req.WorkerSummaryRunID {
				return "", errors.New("worker summary intent missing")
			}
			r.Summary.Text = text
			r.Summary.Identity = tracing.Current(ctx)
			r.Status = "pending"
			if saveErr := saveSummary(ctx, a.Workspace.Root, r); saveErr != nil {
				return "", saveErr
			}
			s.mu.Lock()
			if n := s.workerNotifications[req.Conversation]; n != nil {
				n.summary = WorkerSummary{Conversation: req.Conversation, SessionID: a.SessionID(), RunID: a.LastRunID(), Text: text, Source: req.Source}
			}
			s.mu.Unlock()
		}
		return text, err
	}
	if req.ApprovalID != "" {
		return a.ResumeApproval(ctx, req.ApprovalID, req.Source.SenderID, req.ApprovalAllowed)
	}
	return a.Ask(ctx, req.Text)
}

// DecideApproval is a trusted control-plane operation, never parsed from model
// output or ordinary group text. Source must be supplied by the authenticated
// transport and is checked against the durable conversation binding.
func (s *Service) DecideApproval(ctx context.Context, conversation string, source scheduler.Source, id string, allow bool) (*scheduler.Handle, error) {
	if id == "" {
		return nil, errors.New("approval ID required")
	}
	return s.Scheduler.Submit(ctx, scheduler.Request{Conversation: conversation, Source: source, Text: "approval decision", ApprovalID: id, ApprovalAllowed: allow})
}
func (s *Service) Close(ctx context.Context) error {
	s.stopWorkerNotifications()
	s.Scheduler.Seal()
	// Background children belong to the session, not its foreground turn. Seal
	// their admission and cancel them now rather than waiting for a foreground
	// turn that may itself be waiting for a child result.
	s.mu.Lock()
	for _, a := range s.agents {
		if a.Workers != nil {
			a.Workers.Seal()
		}
	}
	s.mu.Unlock()
	s.closeOnce.Do(func() {
		go func() {
			s.workerWG.Wait()
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

// CancelConversation stops both foreground and background work without closing
// the reusable service. Cancellation is a request; Close/worker Stop waits for
// cleanup before reporting resource reclamation.
func (s *Service) CancelConversation(conversation string) {
	s.cancelWorkerNotification(conversation)
	s.Scheduler.CancelConversation(conversation)
	s.mu.Lock()
	a := s.agents[conversation]
	s.mu.Unlock()
	if a != nil && a.Workers != nil {
		a.Workers.CancelAll()
	}
}
