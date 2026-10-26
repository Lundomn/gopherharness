//go:build !windows

package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFIFOIsRejectedWithoutBlockingOpen(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	done := make(chan error, 1)
	go func() { _, err := w.ReadSnapshot(context.Background(), "pipe"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO treated as regular file")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO open blocked")
	}
}

func TestDirectoryAndSearchNeverBlockOnFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	done := make(chan error, 1)
	go func() {
		if _, err := w.ListFiles(context.Background(), "pipe"); err == nil {
			done <- fmt.Errorf("listed FIFO as directory")
			return
		}
		out, err := w.SearchFiles(context.Background(), "anything", ".")
		if err == nil && !strings.Contains(out, "skipped 1") {
			err = fmt.Errorf("special file not marked: %s", out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("search/list blocked on FIFO")
	}
}
