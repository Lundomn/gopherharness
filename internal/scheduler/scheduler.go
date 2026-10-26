// Package scheduler owns per-conversation turns, safe-point injection, and
// independent foreground/background capacity. Queues are process-local.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Lundomn/gopherharness/internal/tracing"
)

type Policy string

const DepthWarningThreshold = 50

type LaneStatus struct {
	Queued       int
	Injected     int
	Active       bool
	Paused       bool
	DepthWarning bool
	Capacity     int
}

const (
	Append    Policy = "append"
	Inject    Policy = "inject"
	Interrupt Policy = "interrupt"
)

type Source struct {
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	SenderID string `json:"sender_id"`
}
type Request struct {
	Conversation string
	Text         string
	Source       Source
	Background   bool
	Busy         Policy
	// Approval controls are supplied by authenticated host code, not chat text.
	ApprovalID         string
	ApprovalAllowed    bool
	WorkerResults      bool
	WorkerSummaryRunID string
	// Trace is copied into the queue envelope, never read from a global current
	// request. The consumer keeps its own cancellation/deadline ownership.
	Trace tracing.Identity
}
type Result struct {
	Text   string
	Err    error
	Merged bool
}

// Merged is meaningful after Wait returns the task result. Merged requests
// share the owning turn's output and must not trigger a duplicate delivery.
func (h *Handle) Merged() bool { <-h.done; return h.result.Merged }

type Handle struct {
	done   chan struct{}
	cancel context.CancelFunc
	once   sync.Once
	result Result
}

func (h *Handle) Wait(ctx context.Context) (string, error) {
	select {
	case <-h.done:
		return h.result.Text, h.result.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (h *Handle) Cancel()              { h.cancel() }
func (h *Handle) finish(result Result) { h.once.Do(func() { h.result = result; close(h.done) }) }

type Runner func(context.Context, Request, func() []Request) (string, error)
type job struct {
	req    Request
	ctx    context.Context
	handle *Handle
}
type lane struct {
	queue   []*job
	inbox   []*job
	active  *job
	running bool
	paused  bool
	source  Source
}
type Scheduler struct {
	mu                     sync.Mutex
	lanes                  map[string]*lane
	closed                 bool
	run                    Runner
	foreground, background chan struct{}
	capacity               int
	wg                     sync.WaitGroup
	waitOnce               sync.Once
	done                   chan struct{}
}

func New(run Runner, userSlots, systemSlots, queueCapacity int) *Scheduler {
	if run == nil || userSlots < 1 || systemSlots < 1 || queueCapacity < 1 {
		panic("invalid scheduler configuration")
	}
	return &Scheduler{run: run, lanes: map[string]*lane{}, foreground: make(chan struct{}, userSlots), background: make(chan struct{}, systemSlots), capacity: queueCapacity, done: make(chan struct{})}
}

// Status is an observation, not an admission policy. Fifty waiting messages is
// a warning threshold; the independently configured hard queue capacity applies.
func (s *Scheduler) Status(conversation string) (LaneStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lanes[conversation]
	if !ok {
		return LaneStatus{}, false
	}
	return LaneStatus{Queued: len(l.queue), Injected: len(l.inbox), Active: l.active != nil, Paused: l.paused, DepthWarning: len(l.queue)+len(l.inbox) >= DepthWarningThreshold, Capacity: s.capacity}, true
}
func (s *Scheduler) Submit(ctx context.Context, req Request) (*Handle, error) {
	if req.Conversation == "" {
		req.Conversation = req.Source.Channel + ":" + req.Source.ChatID
	}
	if req.Conversation == ":" || req.Text == "" {
		return nil, errors.New("conversation and text required")
	}
	if req.Busy != "" && req.Busy != Append && req.Busy != Inject && req.Busy != Interrupt {
		return nil, fmt.Errorf("unknown busy policy %q", req.Busy)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("scheduler closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := s.lanes[req.Conversation]
	if l == nil {
		l = &lane{source: req.Source}
		s.lanes[req.Conversation] = l
	} else if l.source != req.Source {
		// Reject before INJECT/INTERRUPT can mutate another identity's turn.
		return nil, errors.New("conversation source identity mismatch")
	}
	if len(l.queue)+len(l.inbox) >= s.capacity {
		return nil, errors.New("conversation queue full")
	}
	jobCtx, cancel := context.WithCancel(ctx)
	if req.Trace.TraceID == "" {
		req.Trace = tracing.Current(ctx)
	}
	jobCtx = tracing.Attach(jobCtx, req.Trace)
	h := &Handle{done: make(chan struct{}), cancel: cancel}
	j := &job{req: req, ctx: jobCtx, handle: h}
	if req.ApprovalID != "" {
		l.queue = append([]*job{j}, l.queue...)
	} else if req.Busy == Inject && l.active != nil {
		l.inbox = append(l.inbox, j)
		return h, nil
	} else if req.Busy == Interrupt && l.active != nil {
		l.active.handle.Cancel()
		l.queue = append([]*job{j}, l.queue...)
	} else {
		l.queue = append(l.queue, j)
	}
	if !l.running && (!l.paused || req.ApprovalID != "") {
		l.running = true
		s.wg.Add(1)
		go s.work(req.Conversation, l)
	}
	return h, nil
}
func (s *Scheduler) work(cid string, l *lane) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if len(l.queue) == 0 || (l.paused && l.queue[0].req.ApprovalID == "") {
			l.running = false
			if !l.paused {
				delete(s.lanes, cid)
			}
			s.mu.Unlock()
			return
		}
		j := l.queue[0]
		l.queue = l.queue[1:]
		l.active = j
		s.mu.Unlock()
		pool := s.foreground
		if j.req.Background {
			pool = s.background
		}
		acquired := false
		select {
		case pool <- struct{}{}:
			acquired = true
		case <-j.ctx.Done():
		}
		var consumed []*job
		drain := func() []Request {
			s.mu.Lock()
			defer s.mu.Unlock()
			var requests []Request
			for _, item := range l.inbox {
				if err := item.ctx.Err(); err != nil {
					item.handle.finish(Result{Err: err})
					item.handle.cancel()
					continue
				}
				requests = append(requests, item.req)
				consumed = append(consumed, item)
			}
			l.inbox = nil
			return requests
		}
		var result Result
		if err := j.ctx.Err(); err != nil {
			result.Err = err
		} else {
			result.Text, result.Err = s.invoke(j, drain)
		}
		if acquired {
			<-pool
		}
		j.handle.finish(result)
		j.handle.cancel()
		s.mu.Lock()
		for _, item := range consumed {
			r := result
			r.Merged = true
			if item.ctx.Err() != nil {
				r.Err = item.ctx.Err()
			}
			item.handle.finish(r)
			item.handle.cancel()
		}
		// Messages not consumed before turn end become ordinary queued turns.
		l.queue = append(l.queue, l.inbox...)
		l.inbox = nil
		l.active = nil
		var pause interface{ PauseConversation() bool }
		if errors.As(result.Err, &pause) && pause.PauseConversation() {
			l.paused = true
		} else if j.req.ApprovalID != "" && result.Err == nil {
			l.paused = false
		}
		s.mu.Unlock()
	}
}
func (s *Scheduler) invoke(j *job, drain func() []Request) (text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("runner panic: %v", p)
		}
	}()
	return s.run(j.ctx, j.req, drain)
}
func (s *Scheduler) CancelConversation(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.lanes[cid]; l != nil {
		if l.active != nil {
			l.active.handle.Cancel()
		}
		for _, j := range append(append([]*job{}, l.queue...), l.inbox...) {
			j.handle.Cancel()
			j.handle.finish(Result{Err: context.Canceled})
		}
		l.queue, l.inbox = nil, nil
	}
}

// Close seals admission before cancelling accepted work; repeated calls can wait
// on the same workers. A context timeout does not claim resources were reclaimed.
func (s *Scheduler) Seal() {
	s.mu.Lock()
	s.closed = true
	for _, l := range s.lanes {
		if l.active != nil {
			l.active.handle.Cancel()
		}
		for _, j := range append(append([]*job{}, l.queue...), l.inbox...) {
			j.handle.Cancel()
			j.handle.finish(Result{Err: context.Canceled})
		}
		l.queue, l.inbox = nil, nil
	}
	s.mu.Unlock()
}

func (s *Scheduler) Close(ctx context.Context) error {
	s.Seal()
	s.waitOnce.Do(func() { go func() { s.wg.Wait(); close(s.done) }() })
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
