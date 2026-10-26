package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSnapshotHashesReturnedBytesAndViewsDoNotShareReadAuthorization(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	_ = os.WriteFile(path, []byte("old"), 0600)
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.ReadSnapshot(context.Background(), "file.txt")
	if err != nil || string(got) != "old" {
		t.Fatalf("%s %v", got, err)
	}
	if err := w.View().WriteFile(context.Background(), "file.txt", []byte("from child")); !errors.Is(err, ErrStaleRead) {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, []byte("new"), 0600)
	if err := w.PatchFile(context.Background(), "file.txt", "new", "overwrite"); !errors.Is(err, ErrStaleRead) {
		t.Fatal(err)
	}
	_, _ = w.ReadSnapshot(context.Background(), "file.txt")
	if err := w.PatchFile(context.Background(), "file.txt", "new", "updated"); err != nil {
		t.Fatal(err)
	}
	if w.FreshRead(path) {
		t.Fatal("write left prior read authorization active")
	}
	mode, _ := os.Stat(path)
	if mode.Mode().Perm() != 0600 {
		t.Fatal("existing permission changed")
	}
}
func TestCooperatingConcurrentWritersCannotOverwriteSameSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	_ = os.WriteFile(path, []byte("old"), 0600)
	w, _ := Open(root)
	a, b := w.View(), w.View()
	_, _ = a.ReadSnapshot(context.Background(), "file.txt")
	_, _ = b.ReadSnapshot(context.Background(), "file.txt")
	var success atomic.Int32
	var wg sync.WaitGroup
	for _, view := range []*Workspace{a, b} {
		wg.Add(1)
		go func(view *Workspace) {
			defer wg.Done()
			err := view.WriteFile(context.Background(), "file.txt", []byte("new"))
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrStaleRead) {
				t.Error(err)
			}
		}(view)
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatal("lost update allowed", success.Load())
	}
}
func TestRootedFileOperationsRejectSymlinkEscapeAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	w, _ := Open(root)
	if _, err := w.ReadSnapshot(context.Background(), "link/secret"); err == nil {
		t.Fatal("read escaped")
	}
	if err := w.WriteFile(context.Background(), "link/secret", []byte("bad")); err == nil {
		t.Fatal("write escaped")
	}
	if _, err := w.ReadSnapshot(context.Background(), "."); err == nil {
		t.Fatal("directory read treated as file")
	}
	if err := w.WriteFile(context.Background(), "nested/new.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "nested", "new.txt"))
	if err != nil || string(data) != "new" {
		t.Fatalf("%s %v", data, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "nested"))
	if len(entries) != 1 {
		t.Fatal("temporary file leaked", entries)
	}
}
func TestFingerprintDetectsSameLengthSameMtimeContentChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	_ = os.WriteFile(path, []byte("one"), 0600)
	w, _ := Open(root)
	first, err := w.FingerprintChecked()
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	_ = os.WriteFile(path, []byte("two"), 0600)
	_ = os.Chtimes(path, info.ModTime(), info.ModTime())
	second, err := w.FingerprintChecked()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("metadata-only fingerprint missed content change")
	}
	_ = os.Mkdir(filepath.Join(root, ".pico"), 0700)
	_ = os.WriteFile(filepath.Join(root, ".pico", "ignored"), []byte("internal"), 0600)
	third, err := w.FingerprintChecked()
	if err != nil || second != third {
		t.Fatal("internal metadata affected workspace fingerprint", err)
	}
}
