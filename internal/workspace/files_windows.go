//go:build windows

package workspace

import "os"

func openForRead(root *os.Root, rel string) (*os.File, error) { return root.Open(rel) }
func makeDirectoryPath(root *os.Root, rel string, perm os.FileMode) error {
	return root.MkdirAll(rel, perm)
}

// Windows does not expose directory fsync through os.File.Sync. File data is
// synced before rename; no POSIX directory durability guarantee is claimed.
func syncRootDirectory(*os.Root) error { return nil }
