package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var ignored = map[string]bool{".git": true, ".pico": true, ".venv": true, "node_modules": true, "__pycache__": true, "dist": true, "build": true}

type Workspace struct {
	Root  string
	mu    sync.RWMutex
	reads map[string]string
}

func Open(path string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("workspace is not a directory: %s", real)
	}
	return &Workspace{Root: real, reads: map[string]string{}}, nil
}

func (w *Workspace) Resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	var candidate string
	if filepath.IsAbs(rel) {
		candidate = filepath.Clean(rel)
	} else {
		candidate = filepath.Join(w.Root, filepath.Clean(rel))
	}
	resolved, err := resolveExistingAncestor(candidate)
	if err != nil {
		return "", err
	}
	inside, err := filepath.Rel(w.Root, resolved)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return candidate, nil
}

func resolveExistingAncestor(path string) (string, error) {
	current := path
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("cannot resolve path %s", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
	real, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		real = filepath.Join(real, suffix[i])
	}
	return filepath.Clean(real), nil
}

func (w *Workspace) Relative(path string) string {
	rel, err := filepath.Rel(w.Root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func (w *Workspace) MarkRead(path string) {
	// Compatibility for legacy hosts. New tool reads use ReadSnapshot so the
	// digest is derived from the exact bytes returned, not a second read.
	_, _ = w.ReadSnapshot(context.Background(), path)
}

func (w *Workspace) FreshRead(path string) bool {
	key, rel, err := w.canonical(path)
	if err != nil {
		return false
	}
	data, err := w.read(context.Background(), rel)
	if err != nil {
		return false
	}
	w.mu.RLock()
	prior, ok := w.reads[key]
	w.mu.RUnlock()
	return ok && prior == bytesDigest(data)
}

func (w *Workspace) Invalidate(path string) {
	key, _, err := w.canonical(path)
	if err != nil {
		key = path
	}
	w.mu.Lock()
	delete(w.reads, key)
	w.mu.Unlock()
}

func (w *Workspace) Fingerprint() string {
	digest, err := w.FingerprintChecked()
	if err != nil {
		return ""
	}
	return digest
}

// FingerprintChecked includes content, type and permission metadata. A missing
// or unreadable entry is an error, never silently a matching workspace.
func (w *Workspace) FingerprintChecked() (string, error) {
	r, err := os.OpenRoot(w.Root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := sha256.New()
	err = fs.WalkDir(r.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		parts := strings.Split(path, "/")
		if ignored[parts[0]] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%q:%s:", path, info.Mode().String())
		if d.Type()&os.ModeSymlink != 0 {
			target, err := r.Readlink(filepath.FromSlash(path))
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%q\n", target)
			return nil
		}
		if d.IsDir() {
			fmt.Fprintln(h)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("fingerprint unsupported file type: %s", path)
		}
		f, err := openForRead(r, filepath.FromSlash(path))
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		if !opened.Mode().IsRegular() {
			_ = f.Close()
			return fmt.Errorf("file type changed during fingerprint: %s", path)
		}
		contentHash := sha256.New()
		_, copyErr := io.Copy(contentHash, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintln(h, hex.EncodeToString(contentHash.Sum(nil)))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (w *Workspace) Summary() string {
	entries, _ := os.ReadDir(w.Root)
	var names []string
	for _, e := range entries {
		if !ignored[e.Name()] {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	branch := ""
	if out, err := exec.Command("git", "-C", w.Root, "branch", "--show-current").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	return fmt.Sprintf("Workspace: %s\nBranch: %s\nTop-level: %s", w.Root, branch, strings.Join(names, ", "))
}

func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func ShellEnv() []string {
	allow := map[string]bool{"HOME": true, "PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TMPDIR": true, "USER": true, "LOGNAME": true, "SHELL": true, "GOCACHE": true, "GOMODCACHE": true, "GOPATH": true, "XDG_CACHE_HOME": true}
	var out []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if allow[key] {
			out = append(out, kv)
		}
	}
	return out
}
