package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type inputBackend struct {
	calls, top int
	scope      Scope
	query      string
	err        error
	hits       []Fact
}

func (b *inputBackend) Recall(_ context.Context, s Scope, q string, n int) ([]Fact, error) {
	b.calls++
	b.scope = s
	b.query = q
	b.top = n
	return b.hits, b.err
}
func (*inputBackend) Store(context.Context, Scope, string, []Evidence) error { return nil }
func (*inputBackend) Close() error                                           { return nil }

func TestProfileSelectionMatchesTwoSectionsPlusNotes(t *testing.T) {
	profile := "# User\npreamble\n## Preferences\nGo SQL\n## SQL\nSQL\n## Go\nGo\n## Notes\nalways included\n## Other\nunknown\n"
	text, n, full := SelectProfile(profile, "Go SQL")
	// Titles weigh three times body words: SQL and Go beat Preferences.
	if full || n != 3 || strings.Contains(text, "## Preferences") || !strings.Contains(text, "## Notes") || strings.Index(text, "## SQL") > strings.Index(text, "## Go") {
		t.Fatalf("%d %v %s", n, full, text)
	}
	for _, query := range []string{"", "unmatched"} {
		text, n, full = SelectProfile("## A\nbody\n## B\nother", query)
		if !full || n != 2 || !strings.Contains(text, "## A") {
			t.Fatal(query, n, full, text)
		}
	}
	text, n, full = SelectProfile(profile, "unmatched")
	if full || n != 1 || !strings.Contains(text, "## Notes") || strings.Contains(text, "## SQL") {
		t.Fatal(n, full, text)
	}
	text, _, full = SelectProfile("no H2 profile", "no match")
	if !full || text != "no H2 profile" {
		t.Fatal(text)
	}
	text, _, _ = SelectProfile("## Go\nfirst\n## Go\nlast", "Go")
	if strings.Contains(text, "first") || !strings.Contains(text, "last") {
		t.Fatal(text)
	}
}

func TestCompositeMemoryHasExactlyTwoInputsAndOneHeading(t *testing.T) {
	backend := &inputBackend{hits: []Fact{{ID: "1", Subject: "repo", Attribute: "language", Value: "Go"}}}
	s, err := (InputBuilder{ReadHost: func(context.Context) (string, error) { return "## Projects\nGo agent\n## Notes\nkeep checks", nil }, Backend: backend, Scope: Scope{Repo: "r", User: "u"}, Enabled: true}).Build(context.Background(), "Go")
	if err != nil || backend.calls != 1 || backend.top != 5 || backend.query != "Go" || backend.scope.User != "u" || s.Metadata.Hits != 1 || !s.Metadata.HasHost || strings.Count(s.Text, "# Memory\n") != 1 || !strings.Contains(s.Text, "UNTRUSTED recalled memory") || !strings.Contains(s.Text, "repo.language=Go") {
		t.Fatalf("%+v %v", s, err)
	}
	if strings.Contains(s.Text, "Working memory") {
		t.Fatal("working state leaked into long-term inputs")
	}
}

func TestMemoryInputDisabledEmptyAndHardFailures(t *testing.T) {
	bad := errors.New("backend down")
	b := &inputBackend{err: bad}
	readCalls := 0
	builder := InputBuilder{ReadHost: func(context.Context) (string, error) { readCalls++; return "host", nil }, Backend: b}
	s, err := builder.Build(context.Background(), "q")
	if err != nil || s.Text != "" || b.calls != 0 || readCalls != 0 {
		t.Fatal(s, err)
	}
	builder.Enabled = true
	if _, err = builder.Build(context.Background(), "q"); !errors.Is(err, bad) {
		t.Fatal("hard failure swallowed", err)
	}
	b.err = nil
	builder.ReadHost = func(context.Context) (string, error) { return "", nil }
	s, err = builder.Build(context.Background(), "q")
	if err != nil || s.Text != "" || s.Metadata.Hits != 0 {
		t.Fatal(s, err)
	}
	builder.ReadHost = func(context.Context) (string, error) { return "", bad }
	if _, err = builder.Build(context.Background(), "q"); !errors.Is(err, bad) {
		t.Fatal("host failure swallowed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = builder.Build(ctx, "q"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
