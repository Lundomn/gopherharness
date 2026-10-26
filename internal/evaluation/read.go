package evaluation

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Public datasets and verifier artifacts are data, not trusted file types.
// Pin the parent/root, refuse FIFO/device streams and bound the bytes retained.
func readArtifact(root *os.Root, path string, limit int64) ([]byte, error) {
	f, err := openArtifact(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("artifact must be a regular file of at most %d bytes: %s", limit, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artifact grew beyond %d bytes: %s", limit, path)
	}
	return data, nil
}
func readArtifactPath(path string, limit int64) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readArtifact(r, filepath.Base(abs), limit)
}
