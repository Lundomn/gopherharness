package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/provider"
)

type ownedDreamClient struct{ entered, released chan struct{} }

func (*ownedDreamClient) Name() string  { return "dream-test" }
func (*ownedDreamClient) Model() string { return "dream-test" }
func (c *ownedDreamClient) Complete(ctx context.Context, _ string, _ int) (provider.Completion, error) {
	close(c.entered)
	<-ctx.Done()
	close(c.released)
	return provider.Completion{}, ctx.Err()
}

func TestCloseCancelsAndJoinsManualDream(t *testing.T) {
	c := &ownedDreamClient{make(chan struct{}), make(chan struct{})}
	a, err := New(Options{Root: t.TempDir(), Provider: c, DisableAutoDream: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	done := make(chan error, 1)
	go func() { _, err := a.RunDream(context.Background()); done <- err }()
	select {
	case <-c.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dream did not enter")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.released:
	default:
		t.Fatal("close returned before manual dream stopped")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !a.Memory.LastDreamAt().IsZero() {
		t.Fatal("canceled dream committed")
	}
	if _, err := a.RunDream(context.Background()); err == nil {
		t.Fatal("closed runtime accepted dream")
	}
}
