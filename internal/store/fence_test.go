package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestSessionCASAndOwnerGeneration(t *testing.T) {
	s := NewSessionStore(t.TempDir())
	a := s.New("workspace")
	if e := s.Save(a); e != nil {
		t.Fatal(e)
	}
	old, e := s.Load(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	newOwner, e := s.Load(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Claim(context.Background(), newOwner); e != nil {
		t.Fatal(e)
	}
	if e = s.Save(old); !errors.Is(e, ErrSessionConflict) {
		t.Fatalf("stale writer accepted: %v", e)
	}
	newOwner.Messages = append(newOwner.Messages, model.Message{Role: "user", Content: "accepted"})
	if e = s.Save(newOwner); e != nil {
		t.Fatal(e)
	}
}
func TestSessionContentFenceDetectsEditsWithoutRevisionChange(t *testing.T) {
	s := NewSessionStore(t.TempDir())
	a := s.New("workspace")
	if e := s.Save(a); e != nil {
		t.Fatal(e)
	}
	tampered := *a
	tampered.Messages = []model.Message{{Role: "user", Content: "external edit"}}
	b, _ := json.Marshal(tampered)
	if e := os.WriteFile(filepath.Join(s.Dir, a.ID+".json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if e := s.Save(a); !errors.Is(e, ErrSessionConflict) {
		t.Fatalf("content fence bypassed: %v", e)
	}
}
func TestSessionClaimCannotTakeOverRunningTurn(t *testing.T) {
	s := NewSessionStore(t.TempDir())
	a := s.New("workspace")
	if e := s.Save(a); e != nil {
		t.Fatal(e)
	}
	release, e := s.TurnLease(context.Background(), a.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e = s.Claim(ctx, a); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("running turn taken over: %v", e)
	}
}
func TestSessionSaveRejectsTraversal(t *testing.T) {
	s := NewSessionStore(t.TempDir())
	if s.Save(&model.Session{ID: "../bad"}) == nil {
		t.Fatal("path escape accepted")
	}
}
