package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSearchConfinedBoundedAndDoesNotGrantFreshRead(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle inside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle OUTSIDE_SECRET\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	out, err := w.SearchFiles(context.Background(), "needle", ".")
	if err != nil || !strings.Contains(out, "a.txt:1:needle inside") || strings.Contains(out, "OUTSIDE_SECRET") || !strings.Contains(out, "skipped 1") {
		t.Fatalf("%q %v", out, err)
	}
	if w.FreshRead("a.txt") {
		t.Fatal("search silently authorized a write")
	}
	if _, err := w.SearchFiles(context.Background(), "needle", "link.txt"); err == nil {
		t.Fatal("outside symlink accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(strings.Repeat("needle\n", 250)), 0600); err != nil {
		t.Fatal(err)
	}
	out, err = w.SearchFiles(context.Background(), "needle", "a.txt")
	if err != nil || strings.Count(out, "a.txt:") != 200 || !strings.Contains(out, "partial search") {
		t.Fatalf("bounded search %q %v", out, err)
	}
}

func TestReadListingCapsAndCancellation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 205; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%03d.txt", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := Open(root)
	out, err := w.ListFiles(context.Background(), ".")
	if err != nil || strings.Count(out, "[F]") != 200 || !strings.Contains(out, "truncated") {
		t.Fatalf("%q %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = w.ListFiles(ctx, "."); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = w.SearchFiles(ctx, "x", "."); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSearchMarksOversizedAndBinaryFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large"), []byte(strings.Repeat("x", MaxSearchFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte("needle\x00hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	out, err := w.SearchFiles(context.Background(), "needle", ".")
	if err != nil || !strings.Contains(out, "skipped 2") || strings.Contains(out, "binary:1:") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestSearchPathReplacementCannotReadOutside(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{filepath.Join(inside, "text"): "needle inside", filepath.Join(outside, "text"): "needle OUTSIDE_SECRET"} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			target := inside
			if i%2 == 0 {
				target = outside
			}
			tmp := filepath.Join(root, "swap")
			_ = os.Remove(tmp)
			_ = os.Symlink(target, tmp)
			_ = os.Rename(tmp, alias)
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 200; i++ {
		out, _ := w.SearchFiles(context.Background(), "needle", "alias/text")
		if strings.Contains(out, "OUTSIDE_SECRET") {
			t.Fatal("path replacement escaped root")
		}
	}
}
