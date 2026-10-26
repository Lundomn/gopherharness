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

func TestToolControlPrivacyAndExactPlanException(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(root, ".pico", "plans"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".pico/session.json": "private conversation", ".pico/budgets.json": "cost state", ".pico/plans/active.md": "plan", ".pico/plans/other.md": "other plan"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, ".pico"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	for _, path := range []string{".pico/session.json", "alias/session.json", ".pico/plans/other.md"} {
		if _, err := w.ReadToolSnapshot(ctx, path, ".pico/plans/active.md"); !errors.Is(err, ErrControlPath) {
			t.Fatalf("private read %s: %v", path, err)
		}
		if err := w.WriteToolFile(ctx, path, []byte("bad"), ""); !errors.Is(err, ErrControlPath) {
			t.Fatalf("private write %s: %v", path, err)
		}
	}
	if _, err := w.ListFiles(ctx, ".pico"); !errors.Is(err, ErrControlPath) {
		t.Fatal(err)
	}
	if _, err := w.SearchFiles(ctx, "private", "alias"); !errors.Is(err, ErrControlPath) {
		t.Fatal(err)
	}
	if listing, err := w.ListFiles(ctx, "."); err != nil || strings.Contains(listing, "[D] .pico") {
		t.Fatalf("control directory exposed: %s %v", listing, err)
	}
	if data, err := w.ReadFile(ctx, ".pico/session.json"); err != nil || string(data) != "private conversation" {
		t.Fatal("internal memory access broken", err)
	}
	plan := ".pico/plans/active.md"
	if _, err := w.ReadToolSnapshot(ctx, plan, plan); err != nil {
		t.Fatal(err)
	}
	if err := w.PatchToolFile(ctx, plan, "plan", "updated plan", plan); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".pico", "budgets.json"), filepath.Join(root, ".pico", "plans", "budget-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadToolSnapshot(ctx, ".pico/plans/budget-alias", ".pico/plans/budget-alias"); !errors.Is(err, ErrControlPath) {
		t.Fatal("plan alias escaped plans", err)
	}
}

func TestToolPathRaceCannotRedirectReadsOrNewWritesIntoControlTree(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "safe")
	private := filepath.Join(root, ".pico")
	for _, dir := range []string{inside, private} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{filepath.Join(inside, "data"): "public", filepath.Join(private, "data"): "PRIVATE"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(inside, alias); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	ctx := context.Background()
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
				target = private
			}
			tmp := filepath.Join(root, "swap")
			_ = os.Remove(tmp)
			_ = os.Symlink(target, tmp)
			_ = os.Rename(tmp, alias)
		}
	}()
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 200; i++ {
		data, _ := w.ReadToolSnapshot(ctx, "alias/data", "")
		if strings.Contains(string(data), "PRIVATE") {
			t.Fatal("private bytes leaked")
		}
		_ = w.WriteToolFile(ctx, fmt.Sprintf("alias/new-%d", i), []byte("user write"), "")
	}
	entries, err := os.ReadDir(private)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "new-") {
			t.Fatal("write redirected into control tree", e.Name())
		}
	}
}
