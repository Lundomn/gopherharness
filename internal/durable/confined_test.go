package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceWithinRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceWithin(root, "escape/file", []byte("bad")); err == nil {
		t.Fatal("followed symlink outside root")
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := ReplaceWithin(root, "nested/file", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "nested/file")); err != nil || string(got) != "ok" {
		t.Fatal(string(got), err)
	}
}
