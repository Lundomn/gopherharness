package runtime

import (
	"context"
	"time"

	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/store"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

type WorkerSummary struct {
	Conversation, SessionID, RunID, Text string
	Source                               scheduler.Source
	Identity                             tracing.Identity
}
type workerNotification struct {
	scheduled  bool
	generation uint64
	cancel     context.CancelFunc
	summary    WorkerSummary
}

// A channel adapter may hand this summary to its durable delivery queue. The
// runtime itself never guesses an external chat destination or sends a message.
func (s *Service) SetWorkerSummarySink(sink func(context.Context, WorkerSummary) error) {
	s.mu.Lock()
	s.workerSink = sink
	s.mu.Unlock()
	select {
	case s.summaryWake <- struct{}{}:
	default:
	}
}
func (s *Service) wakeWorker(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workerClosed || s.agents[cid] == nil {
		return
	}
	n := s.workerNotifications[cid]
	if n == nil {
		n = &workerNotification{}
		s.workerNotifications[cid] = n
	}
	if n.scheduled {
		return
	}
	n.scheduled = true
	select {
	case s.workerWake <- cid:
	default:
		n.scheduled = false
	}
}
func (s *Service) workerNotificationLoop() {
	defer s.workerWG.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.workerCtx.Done():
			return
		case cid := <-s.workerWake:
			s.workerWG.Add(1)
			go func() { defer s.workerWG.Done(); s.dispatchWorkerSummary(cid) }()
		case <-s.summaryWake:
			s.replayDetachedSummaries()
		case <-ticker.C:
			s.mu.Lock()
			agents := map[string]*Agent{}
			for cid, a := range s.agents {
				agents[cid] = a
			}
			s.mu.Unlock()
			for cid, a := range agents {
				if a.Workspace == nil {
					continue
				}
				if _, ok, err := nextSummary(s.workerCtx, a.Workspace.Root, cid); err == nil && ok {
					s.wakeWorker(cid)
				}
				if a.Workers != nil {
					pending, err := a.Workers.PendingResults(s.workerCtx)
					if err == nil && len(pending) > 0 {
						s.wakeWorker(cid)
					}
				}
			}
			s.replayDetachedSummaries()
		}
	}
}
func (s *Service) dispatchWorkerSummary(cid string) {
	s.mu.Lock()
	n := s.workerNotifications[cid]
	a := s.agents[cid]
	source := s.owners[cid]
	if s.workerClosed || n == nil || a == nil {
		s.mu.Unlock()
		return
	}
	generation := n.generation
	ctx, cancel := context.WithCancel(s.workerCtx)
	n.cancel = cancel
	s.mu.Unlock()
	defer cancel()
	defer func() {
		s.mu.Lock()
		if n.generation == generation {
			n.scheduled = false
			n.cancel = nil
		}
		s.mu.Unlock()
	}()
	r, found, err := nextSummary(ctx, a.Workspace.Root, cid)
	if err != nil {
		return
	}
	if !found {
		pending, err := a.Workers.PendingResults(ctx)
		if err != nil || len(pending) == 0 {
			return
		}
		r = summaryReceipt{Summary: WorkerSummary{Conversation: cid, SessionID: a.SessionID(), RunID: store.NewID("run"), Source: source, Identity: pending[0].Trace}, Status: "generating", CreatedAt: time.Now().UTC(), Results: pending}
		if err = saveSummary(ctx, a.Workspace.Root, r); err != nil {
			return
		}
		req := scheduler.Request{Conversation: cid, Source: source, Text: "A background worker has completed; incorporate its recorded result at the next safe point.", Background: true, Busy: scheduler.Inject, WorkerResults: true, WorkerSummaryRunID: r.Summary.RunID, Trace: pending[0].Trace}
		h, err := s.Scheduler.Submit(ctx, req)
		if err != nil {
			return
		}
		text, err := h.Wait(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil && (h.Merged() || text == "") {
			_ = settleSummary(ctx, a.Workspace.Root, r.Summary.RunID, "accepted", "")
			return
		}
		r, found, err = nextSummary(ctx, a.Workspace.Root, cid)
		if err != nil || !found {
			return
		}
	}
	if r.Summary.SessionID != a.SessionID() || r.Summary.Source != source {
		return
	}
	r, err = recoverSummary(ctx, a.Workspace.Root, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	sink := s.workerSink
	valid := !s.workerClosed && n.generation == generation
	summary := r.Summary
	s.mu.Unlock()
	if valid && sink != nil {
		if err := sink(ctx, summary); err != nil {
			_ = settleSummary(ctx, a.Workspace.Root, summary.RunID, "pending", a.Redactor.Text(err.Error()))
		} else {
			_ = settleSummary(ctx, a.Workspace.Root, summary.RunID, "accepted", "")
		}
	}
}
func (s *Service) cancelWorkerNotification(cid string) {
	s.mu.Lock()
	n := s.workerNotifications[cid]
	a := s.agents[cid]
	if n != nil {
		n.generation++
		n.scheduled = false
		if n.cancel != nil {
			n.cancel()
			n.cancel = nil
		}
	}
	s.mu.Unlock()
	if a != nil && a.Workers != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.Workers.RevokeNotifications(ctx)
		if a.Workspace != nil {
			_ = revokeSummaries(ctx, a.Workspace.Root, cid)
		}
		cancel()
	}
}
func (s *Service) stopWorkerNotifications() {
	s.mu.Lock()
	if s.workerClosed {
		s.mu.Unlock()
		return
	}
	s.workerClosed = true
	s.workerCancel()
	agents := map[string]*Agent{}
	for cid, a := range s.agents {
		agents[cid] = a
	}
	for _, n := range s.workerNotifications {
		n.generation++
		if n.cancel != nil {
			n.cancel()
		}
	}
	s.mu.Unlock()
	for cid, a := range agents {
		if a.Workers != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = a.Workers.RevokeNotifications(ctx)
			if a.Workspace != nil {
				_ = revokeSummaries(ctx, a.Workspace.Root, cid)
			}
			cancel()
		}
	}
}
