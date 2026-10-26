package permission

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func approvalAction() Action {
	return Action{Principal: "user-a", Session: "session-a", Turn: "turn-a", CallID: "call-a", Tool: "write_file", Args: map[string]any{"path": "a.txt", "content": "old", "large": int64(9007199254740993)}, DefinitionDigest: "schema-v1", Preconditions: map[string]string{"a.txt": "sha-old"}}
}
func TestApprovalFreezesActionAndBindsIdentityAndVersion(t *testing.T) {
	s := OpenApprovals(t.TempDir())
	ctx := context.Background()
	a := approvalAction()
	r, err := s.Create(ctx, a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a.Args["content"] = "mutated"
	stored, err := s.Get(ctx, r.ID, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Action.Args["content"] != "old" {
		t.Fatal("caller mutated frozen action")
	}
	if _, err = s.Decide(ctx, r.ID, "other-user", true); !errors.Is(err, ErrApprovalIdentity) {
		t.Fatal(err)
	}
	if _, err = s.Decide(ctx, r.ID, "user-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, r.ID, "user-a", a); !errors.Is(err, ErrApprovalChanged) {
		t.Fatal(err)
	}
	a = approvalAction()
	a.Preconditions["a.txt"] = "new-sha"
	if _, err = s.Claim(ctx, r.ID, "user-a", a); !errors.Is(err, ErrApprovalChanged) {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, r.ID, "user-a", approvalAction())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, "completed", "write verified"); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, "completed", "write verified"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, r.ID, "user-a", approvalAction()); !errors.Is(err, ErrApprovalState) {
		t.Fatal(err)
	}
}
func TestApprovalConcurrentClaimExactlyOneLocalDispatch(t *testing.T) {
	s := OpenApprovals(t.TempDir())
	ctx := context.Background()
	a := approvalAction()
	r, err := s.Create(ctx, a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Decide(ctx, r.ID, "user-a", true); err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Claim(ctx, r.ID, "user-a", a); err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrApprovalState) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatal(won.Load())
	}
	// Reopening cannot turn a claim with unknown outcome into a new dispatch.
	reopened := OpenApprovals(t.TempDir())
	reopened.File = s.File
	if _, err := reopened.Claim(ctx, r.ID, "user-a", a); !errors.Is(err, ErrApprovalState) {
		t.Fatal(err)
	}
	if pending, err := reopened.Pending(ctx, "user-a", "session-a"); err != nil || len(pending) != 0 {
		t.Fatalf("pending=%v %v", pending, err)
	}
}
func TestApprovalExpiryAndUnknownNeverAutoResume(t *testing.T) {
	s := OpenApprovals(t.TempDir())
	now := time.Now()
	s.Now = func() time.Time { return now }
	ctx := context.Background()
	a := approvalAction()
	r, err := s.Create(ctx, a, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Decide(ctx, r.ID, "user-a", true)
	now = now.Add(time.Second)
	if _, err = s.Claim(ctx, r.ID, "user-a", a); !errors.Is(err, ErrApprovalExpired) {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, r.ID, "user-a")
	if got.Status != "expired" {
		t.Fatal(got.Status)
	}
	a.CallID = "call-b"
	r, err = s.Create(ctx, a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Decide(ctx, r.ID, "user-a", true)
	claim, err := s.Claim(ctx, r.ID, "user-a", a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, "unknown", "network receipt lost"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, r.ID, "user-a", a); !errors.Is(err, ErrApprovalState) {
		t.Fatal(err)
	}
}
