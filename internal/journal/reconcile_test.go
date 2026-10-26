package journal

import (
	"context"
	"testing"
)

func TestUnknownOutboxNeedsExplicitAuditedDecision(t *testing.T) {
	ctx := context.Background()
	j, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	_, _ = j.Put(ctx, "outbox", "id", map[string]string{"text": "result"})
	if err := j.Move(ctx, "outbox", "id", "pending", "unknown", nil); err != nil {
		t.Fatal(err)
	}
	decision := Resolution{Action: "confirmed_not_sent", Actor: "operator", Reason: "verify network failure", Evidence: "platform query found no receipt"}
	bad := decision
	bad.Evidence = ""
	if err := j.Resolve(ctx, "outbox", "id", bad, nil); err == nil {
		t.Fatal("unverified retry accepted")
	}
	if err := j.Resolve(ctx, "outbox", "id", decision, nil); err != nil {
		t.Fatal(err)
	}
	if err := j.Resolve(ctx, "outbox", "id", decision, nil); err == nil {
		t.Fatal("stale decision accepted twice")
	}
	entries, err := j.List(ctx, "outbox", "pending")
	if err != nil || len(entries) != 1 || len(entries[0].Resolutions) != 1 || entries[0].Resolutions[0].At.IsZero() {
		t.Fatal(entries, err)
	}
}

func TestRecoveredInboundResultAndOutboxCommitTogether(t *testing.T) {
	ctx := context.Background()
	j, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	_, _ = j.Put(ctx, "inbox", "id", map[string]string{"request": "original"})
	_ = j.Move(ctx, "inbox", "id", "pending", "needs_reconcile", nil)
	d := Resolution{Action: "recover_result", Actor: "operator", Reason: "recovered saved result", Evidence: "verified session final message"}
	if err := j.Resolve(ctx, "inbox", "id", d, nil); err == nil {
		t.Fatal("empty result accepted")
	}
	if err := j.Resolve(ctx, "inbox", "id", d, map[string]string{"text": "verified result"}); err != nil {
		t.Fatal(err)
	}
	in, _ := j.List(ctx, "inbox", "completed")
	out, _ := j.List(ctx, "outbox", "pending")
	if len(in) != 1 || len(out) != 1 || string(in[0].Result) != string(out[0].Payload) {
		t.Fatal(in, out)
	}
}
