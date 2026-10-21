package memory

import (
	"context"
	"errors"
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
