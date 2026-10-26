package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDurableWorkerReceiptLeaseAndQuota(t *testing.T) {
	opts := Options{Root: t.TempDir(), Session: "session", Principal: "alice", HourlyLimit: 1, Redact: func(s string) string { return strings.ReplaceAll(s, "secret", "[redacted]") }}
	m, err := NewDurable(func(context.Context, string, string, <-chan string) (string, error) { return "secret result", nil }, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if other, err := NewDurable(m.runner, opts); err == nil {
		other.Close()
		t.Fatal("live owner was stolen")
	}
	id, err := m.Start("explore", "secret request")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	done := m.workers[id].done
	m.mu.RUnlock()
	<-done
	receipts, err := m.PendingResults(context.Background())
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%+v %v", receipts, err)
	}
	if strings.Contains(receipts[0].Result+receipts[0].Prompt, "secret") {
		t.Fatal("receipt leaked credential")
	}
	if err = m.Acknowledge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if r, _ := m.PendingResults(context.Background()); len(r) != 0 {
		t.Fatal("ack did not consume receipt")
	}
	m.Close()
	restored, err := NewDurable(m.runner, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if len(restored.List()) != 1 {
		t.Fatal("worker history disappeared")
	}
	if _, err = restored.Start("explore", "second"); !errors.Is(err, ErrQuota) {
		t.Fatalf("restart reset hourly quota: %v", err)
	}
}
func TestDurableWorkerOrphanIsNotRestartedAndCloseRevokes(t *testing.T) {
	opts := Options{Root: t.TempDir(), Session: "session", Principal: "alice"}
	m, err := NewDurable(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.results.Update(context.Background(), func(s *resultState) error {
		s.Records["orphan"] = Receipt{ID: "orphan", Status: "running", CreatedAt: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Model process death: release the OS owner without orderly revocation.
	m.releaseResults()
	m.releaseResults = nil
	m.cancel()
	restored, err := NewDurable(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	r, err := restored.PendingResults(context.Background())
	if err != nil || len(r) != 1 || r[0].Status != "interrupted" {
		t.Fatalf("orphan=%+v %v", r, err)
	}
	restored.Close()
	next, err := NewDurable(nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if r, _ := next.PendingResults(context.Background()); len(r) != 0 {
		t.Fatal("orderly close resurrected revoked notification")
	}
}
func TestWorkerInboxDefaultFifty(t *testing.T) {
	m := New(func(ctx context.Context, _, _ string, _ <-chan string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	defer m.Close()
	id, err := m.Start("explore", "wait")
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if err = m.Send(id, "follow up"); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Send(id, "overflow"); err == nil {
		t.Fatal("inbox exceeded 50")
	}
}
