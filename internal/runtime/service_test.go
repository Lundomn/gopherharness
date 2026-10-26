package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/worker"
)

type injectionProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	prompts []string
}

func TestServiceStopIncludesBackgroundChildrenAndCanStartAgain(t *testing.T) {
	started := make(chan struct{}, 2)
	m := worker.New(func(ctx context.Context, _, _ string, _ <-chan string) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	s := NewService(Options{}, 1, 1)
	s.agents["conversation"] = &Agent{Workers: m}
	if _, err := m.Start("explore", "first"); err != nil {
		t.Fatal(err)
	}
	<-started
	s.CancelConversation("conversation")
	if _, err := m.Start("explore", "next"); err != nil {
		t.Fatal("stop closed reusable session", err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.RunningIDs()) != 0 {
		t.Fatal("background child survived service shutdown")
	}
}

func TestServiceResumesBoundConversationAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	source := scheduler.Source{Channel: "test", ChatID: "group", SenderID: "alice"}
	opts := Options{Root: root, Provider: &provider.Fake{Responses: []string{"<final>first answer</final>"}}, DisableAutoDream: true}
	s := NewService(opts, 2, 1)
	h, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "group-alice", Text: "first request", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	sessionID := s.agents["group-alice"].SessionID()
	s.mu.Unlock()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	opts.Provider = &provider.Fake{Responses: []string{"<final>second answer</final>"}}
	resumed := NewService(opts, 2, 1)
	defer resumed.Close(ctx)
	h, err = resumed.Scheduler.Submit(ctx, scheduler.Request{Conversation: "group-alice", Text: "second request", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	resumed.mu.Lock()
	agent := resumed.agents["group-alice"]
	resumed.mu.Unlock()
	if agent.SessionID() != sessionID {
		t.Fatal("conversation resumed into unrelated session")
	}
	var found bool
	for _, message := range agent.Session.Messages {
		if message.Content == "first request" {
			found = true
		}
	}
	if !found {
		t.Fatal("persisted conversation history was lost")
	}
	wrong := source
	wrong.SenderID = "mallory"
	h, err = resumed.Scheduler.Submit(ctx, scheduler.Request{Conversation: "group-alice", Text: "steal history", Source: wrong})
	if err == nil {
		_, err = h.Wait(ctx)
	}
	if err == nil {
		t.Fatal("different sender reused bound session")
	}
}

func (p *injectionProvider) Name() string  { return "fake" }
func (p *injectionProvider) Model() string { return "fake" }
func (p *injectionProvider) Complete(ctx context.Context, prompt string, _ int) (provider.Completion, error) {
	p.mu.Lock()
	p.prompts = append(p.prompts, prompt)
	n := len(p.prompts)
	p.mu.Unlock()
	if n == 1 {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return provider.Completion{}, ctx.Err()
		}
		return provider.Completion{Text: `<tool>{"name":"todo_list","args":{}}</tool>`}, nil
	}
	return provider.Completion{Text: "<final>done</final>"}, nil
}
func TestServiceInjectionReachesNextModelIteration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := &injectionProvider{started: make(chan struct{}), release: make(chan struct{})}
	s := NewService(Options{Root: t.TempDir(), Provider: p, DisableAutoDream: true}, 2, 1)
	h, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "a", Text: "original"})
	if err != nil {
		t.Fatal(err)
	}
	<-p.started
	i, err := s.Scheduler.Submit(ctx, scheduler.Request{Conversation: "a", Text: "INJECTED_FACT", Busy: scheduler.Inject})
	if err != nil {
		t.Fatal(err)
	}
	close(p.release)
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = i.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if !i.Merged() {
		t.Fatal("injection receipt not marked merged")
	}
	p.mu.Lock()
	if len(p.prompts) != 2 || strings.Contains(p.prompts[0], "INJECTED_FACT") || !strings.Contains(p.prompts[1], "INJECTED_FACT") {
		t.Error("message injected at wrong boundary")
	}
	p.mu.Unlock()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
