//go:build !windows

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK prevents a replaced FIFO/device from hanging before f.Stat can
// reject it. Ordinary regular-file reads retain their normal semantics.
func openForRead(root *os.Root, rel string) (*os.File, error) {
	return openPathNoFollow(root, rel, false, 0)
}
func makeDirectoryPath(root *os.Root, rel string, perm os.FileMode) error {
	f, err := openPathNoFollow(root, rel, true, perm)
	if err != nil {
		return err
	}
	return f.Close()
}
func openPathNoFollow(root *os.Root, rel string, createDirectory bool, perm os.FileMode) (*os.File, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes workspace: %s", rel)
	}
	current, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_NOFOLLOW
		if i < len(parts)-1 || createDirectory {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(current.Fd()), part, flags, 0)
		if err == unix.ENOENT && createDirectory {
			if mkdirErr := unix.Mkdirat(int(current.Fd()), part, uint32(perm.Perm())); mkdirErr != nil && mkdirErr != unix.EEXIST {
				current.Close()
				return nil, mkdirErr
			}
			fd, err = unix.Openat(int(current.Fd()), part, flags, 0)
		}
		current.Close()
		if err != nil {
			return nil, &os.PathError{Op: "openat", Path: rel, Err: err}
		}
		current = os.NewFile(uintptr(fd), rel)
	}
	return current, nil
}
func syncRootDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
