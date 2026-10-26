//go:build !windows

package evaluation

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPublicArtifactReadRejectsFIFOAndEscapingSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("hidden"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readArtifactPath(filepath.Join(root, "fifo"), 1024); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked artifact reader")
	}
	if _, err := readArtifactPath(filepath.Join(root, "link"), 1024); err == nil {
		t.Fatal("artifact followed escaping symlink")
	}
}
