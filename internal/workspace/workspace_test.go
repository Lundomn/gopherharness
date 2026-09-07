package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRejectsParentAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Resolve("../secret.txt"); err == nil {
		t.Fatal("parent escape allowed")
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Resolve("link/secret.txt"); err == nil {
		t.Fatal("symlink escape allowed")
	}
}
func TestFreshReadTracksDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	_ = os.WriteFile(path, []byte("one"), 0o600)
	w, _ := Open(root)
	w.MarkRead(path)
	if !w.FreshRead(path) {
		t.Fatal("expected fresh")
	}
	_ = os.WriteFile(path, []byte("two"), 0o600)
	if w.FreshRead(path) {
		t.Fatal("changed file remained fresh")
	}
}
