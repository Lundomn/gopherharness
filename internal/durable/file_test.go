package durable

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentTransactionsAndCorruptionFailClosed(t *testing.T) {
	type state struct{ Count int }
	f := File[state]{Path: filepath.Join(t.TempDir(), "state.json")}
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Update(context.Background(), func(s *state) error { s.Count++; return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, err := f.Read(context.Background())
	if err != nil || s.Count != 40 {
		t.Fatal(s, err)
	}
	if err := os.WriteFile(f.Path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(context.Background(), func(s *state) error { s.Count++; return nil }); err == nil {
		t.Fatal("corruption silently overwritten")
	}
}
