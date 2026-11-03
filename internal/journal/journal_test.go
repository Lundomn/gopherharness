package journal

import (
	"bytes"
	"context"
	"os"
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

func TestClosedJournalCannotMutateBeforeNextOwnerOpens(t *testing.T) {
	for _, operation := range []string{"put", "move", "complete", "resolve"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			j, err := Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(j.Close)
			if _, err := j.Put(ctx, "inbox", "request", "original"); err != nil {
				t.Fatal(err)
			}
			if err := j.Move(ctx, "inbox", "request", "pending", "running", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := j.Put(ctx, "outbox", "reply", "answer"); err != nil {
				t.Fatal(err)
			}
			if err := j.Move(ctx, "outbox", "reply", "pending", "unknown", nil); err != nil {
				t.Fatal(err)
			}
			j.Close()
			j.Close()
			before, err := os.ReadFile(j.File.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "put":
				_, err = j.Put(ctx, "inbox", "late", "must not be accepted")
			case "move":
				err = j.Move(ctx, "inbox", "request", "running", "failed", nil)
			case "complete":
				err = j.Complete(ctx, "request", "late result")
			case "resolve":
				err = j.Resolve(ctx, "outbox", "reply", Resolution{Action: "confirmed_not_sent", Actor: "operator", Reason: "verified", Evidence: "provider query"}, nil)
			}
			if err != ErrFence {
				t.Errorf("closed owner mutation error = %v, want %v", err, ErrFence)
			}
			after, readErr := os.ReadFile(j.File.Path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Errorf("closed owner changed durable state: read error %v", readErr)
			}
			next, err := Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if _, err := next.Put(ctx, "inbox", "new-owner", "accepted"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
