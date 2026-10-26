//go:build !windows

package skills

import (
	"os"
	"syscall"
)

func openSkillFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
