package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type measuredFake struct{}

func (measuredFake) Name() string  { return "fake" }
func (measuredFake) Model() string { return "m" }
func (measuredFake) Complete(ctx context.Context, _ string, _ int) (Completion, error) {
	if ctx.Err() != nil {
		return Completion{}, ctx.Err()
	}
	return Completion{Text: "answer", Usage: Usage{InputTokens: 200, CacheReadTokens: 800, OutputTokens: 10, Complete: true}}, nil
}

func TestUsageNormalizationMissingAndCacheAccounting(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		includes    bool
		fresh, read int
		complete    bool
	}{
		{`{}`, true, 0, 0, false},
		{`{"input_tokens":1000,"output_tokens":10,"input_tokens_details":{"cached_tokens":800}}`, true, 200, 800, true},
		{`{"input_tokens":200,"output_tokens":10,"cache_read_input_tokens":800}`, false, 200, 800, true},
		{`{"input_tokens":0,"output_tokens":0}`, true, 0, 0, true},
		{`{"input_tokens":1,"output_tokens":0,"input_tokens_details":{"cached_tokens":2}}`, true, -1, 2, false},
	} {
		var wire WireUsage
		if err := json.Unmarshal([]byte(tc.raw), &wire); err != nil {
			t.Fatal(err)
		}
		u := wire.Normalized(tc.includes)
		if u.InputTokens != tc.fresh || u.CacheReadTokens != tc.read || u.Complete != tc.complete {
			t.Fatalf("usage=%+v", u)
		}
	}
}

func TestAttemptLedgerIncludesCancellationUnknownAndHealth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.jsonl")
	l, err := OpenLedger(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	m := Meter(measuredFake{}, l, map[string]Price{"m": {Input: 1, Output: 2, CacheRead: 0.1}})
	if _, err := m.Complete(context.Background(), "secret prompt not to persist", 20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = m.Complete(ctx, "q", 20)
	unknown := Meter(measuredFake{}, l, nil)
	_, _ = unknown.Complete(context.Background(), "q", 20)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	health := l.Health()
	if health.Accepted != 3 || health.Persisted != 3 || health.Lost != 0 || !health.Closed {
		t.Fatal(health)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret prompt") {
		t.Fatal("prompt leaked")
	}
	var records []CallRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r CallRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if records[0].Cost == nil || records[1].Outcome != "cancelled" || records[1].Cost != nil || records[2].Cost != nil {
		t.Fatalf("records=%+v", records)
	}
	if _, err := os.Stat(path + ".health.json"); err != nil {
		t.Fatal(err)
	}
}
