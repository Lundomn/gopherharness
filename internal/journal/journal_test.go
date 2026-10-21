package journal

import (
	"context"
	"testing"
)

func TestRestartReplaysOnlyPendingAndFencesOldOwner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	j, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = j.Put(ctx, "inbox", "accepted", map[string]string{"text": "q"})
	_, _ = j.Put(ctx, "inbox", "running", map[string]string{"text": "q"})
	_ = j.Move(ctx, "inbox", "running", "pending", "running", nil)
	_, _ = j.Put(ctx, "inbox", "done", map[string]string{"text": "q"})
	_ = j.Move(ctx, "inbox", "done", "pending", "running", nil)
	if err := j.Complete(ctx, "done", map[string]string{"text": "answer"}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	next, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	ready, _ := next.List(ctx, "inbox", "pending")
	uncertain, _ := next.List(ctx, "inbox", "needs_reconcile")
	out, _ := next.List(ctx, "outbox", "pending")
	if len(ready) != 1 || len(uncertain) != 1 || len(out) != 1 {
		t.Fatal(ready, uncertain, out)
	}
	if _, err := j.Put(ctx, "inbox", "stale", nil); err != ErrFence {
		t.Fatal("old owner wrote", err)
	}
}
