package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFactScopeConflictAuthorityAndCAS(t *testing.T) {
	b := OpenBackend(t.TempDir(), nil)
	ctx := context.Background()
	scope := Scope{Repo: "a", User: "u"}
	put := func(value, kind string, expected *uint64) error {
		return b.Upsert(ctx, scope, []Candidate{{Subject: "project", Attribute: "database", Value: value, EvidenceIDs: []string{"e"}}}, []Evidence{{ID: "e", Kind: kind, Text: value, ObservedAt: time.Now()}}, expected)
	}
	if err := put("mysql", "user_confirmed", nil); err != nil {
		t.Fatal(err)
	}
	if err := put("postgres", "model", nil); err != nil {
		t.Fatal(err)
	}
	facts, _ := b.Recall(ctx, scope, "", 5)
	if len(facts) != 1 || facts[0].Value != "mysql" {
		t.Fatal(facts)
	}
	if err := put("postgres", "user_confirmed", nil); err != nil {
		t.Fatal(err)
	}
	facts, _ = b.Recall(ctx, scope, "", 5)
	if len(facts) != 1 || facts[0].Value != "postgres" || facts[0].Supersedes == "" {
		t.Fatal(facts)
	}
	other, _ := b.Recall(ctx, Scope{Repo: "b", User: "u"}, "", 5)
	if len(other) != 0 {
		t.Fatal("cross-project contamination")
	}
	old := uint64(0)
	if err := put("bad", "user_confirmed", &old); !errors.Is(err, ErrRevision) {
		t.Fatal(err)
	}
}

func TestConcurrentDifferentTurnsMergeWithoutRepeatingExtraction(t *testing.T) {
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	b := OpenBackend(t.TempDir(), func(_ context.Context, e []Evidence) ([]Candidate, error) {
		ready <- struct{}{}
		<-release
		return []Candidate{{Subject: e[0].ID, Attribute: "language", Value: "Go", EvidenceIDs: []string{e[0].ID}}}, nil
	})
	scope := Scope{Repo: "project", User: "user"}
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := b.Store(context.Background(), scope, id, []Evidence{{ID: id, Kind: "user_confirmed", Text: "Go"}}); err != nil {
				t.Error(err)
			}
		}(id)
	}
	<-ready
	<-ready
	close(release)
	wg.Wait()
	facts, err := b.Recall(context.Background(), scope, "", 10)
	if err != nil || len(facts) != 2 {
		t.Fatalf("%v %v", facts, err)
	}
}
