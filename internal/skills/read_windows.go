//go:build windows

package skills

import "os"

func openSkillFile(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
