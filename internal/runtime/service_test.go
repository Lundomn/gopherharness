package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

type injectionProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	prompts []string
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
