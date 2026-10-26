package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const MaxFileBytes = 64 << 20

var pathLocks sync.Map
var ErrStaleRead = errors.New("file requires a fresh read_file before modification")

// View isolates read authorization across agents, while package-wide path
// locks still coordinate writers that operate through different views.
func (w *Workspace) View() *Workspace { return &Workspace{Root: w.Root, reads: map[string]string{}} }

func (w *Workspace) canonical(path string) (string, string, error) {
	p, err := w.Resolve(path)
	if err != nil {
		return "", "", err
	}
	key, err := resolveExistingAncestor(p)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(w.Root, key)
	if err != nil {
		return "", "", err
	}
	return key, rel, nil
}
func lockPath(ctx context.Context, key string) (func(), error) {
	v, _ := pathLocks.LoadOrStore(key, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func bytesDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func readRegular(ctx context.Context, root *os.Root, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openForRead(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", rel)
	}
	if st.Size() > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d-byte read limit", MaxFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d-byte read limit", MaxFileBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
func (w *Workspace) read(ctx context.Context, rel string) ([]byte, error) {
	r, err := os.OpenRoot(w.Root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readRegular(ctx, r, rel)
}
func (w *Workspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	_, rel, err := w.canonical(path)
	if err != nil {
		return nil, err
	}
	return w.read(ctx, rel)
}
func (w *Workspace) ReadSnapshot(ctx context.Context, path string) ([]byte, error) {
	key, rel, err := w.canonical(path)
	if err != nil {
		return nil, err
	}
	return w.readSnapshotAt(ctx, key, rel)
}
func (w *Workspace) ReadToolSnapshot(ctx context.Context, path, activePlan string) ([]byte, error) {
	key, rel, err := w.toolCanonical(path, activePlan)
	if err != nil {
		return nil, err
	}
	return w.readSnapshotAt(ctx, key, rel)
}
func (w *Workspace) readSnapshotAt(ctx context.Context, key, rel string) ([]byte, error) {
	release, err := lockPath(ctx, key)
	if err != nil {
		return nil, err
	}
	defer release()
	data, err := w.read(ctx, rel)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.reads[key] = bytesDigest(data)
	w.mu.Unlock()
	return data, nil
}

func (w *Workspace) WriteFile(ctx context.Context, path string, content []byte) error {
	return w.modify(ctx, path, false, func([]byte) ([]byte, error) { return content, nil })
}
func (w *Workspace) WriteToolFile(ctx context.Context, path string, content []byte, activePlan string) error {
	return w.modifyWithPolicy(ctx, path, false, func([]byte) ([]byte, error) { return content, nil }, &activePlan)
}
func (w *Workspace) PatchToolFile(ctx context.Context, path, oldText, newText, activePlan string) error {
	if oldText == "" {
		return errors.New("old_text is required")
	}
	return w.modifyWithPolicy(ctx, path, true, func(data []byte) ([]byte, error) {
		n := bytes.Count(data, []byte(oldText))
		if n != 1 {
			return nil, fmt.Errorf("old_text must occur exactly once, found %d", n)
		}
		return bytes.Replace(data, []byte(oldText), []byte(newText), 1), nil
	}, &activePlan)
}
func (w *Workspace) PatchFile(ctx context.Context, path, oldText, newText string) error {
	if oldText == "" {
		return errors.New("old_text is required")
	}
	return w.modify(ctx, path, true, func(data []byte) ([]byte, error) {
		n := bytes.Count(data, []byte(oldText))
		if n != 1 {
			return nil, fmt.Errorf("old_text must occur exactly once, found %d", n)
		}
		return bytes.Replace(data, []byte(oldText), []byte(newText), 1), nil
	})
}

func (w *Workspace) modify(ctx context.Context, path string, required bool, change func([]byte) ([]byte, error)) error {
	return w.modifyWithPolicy(ctx, path, required, change, nil)
}
func (w *Workspace) modifyWithPolicy(ctx context.Context, path string, required bool, change func([]byte) ([]byte, error), activePlan *string) error {
	var key, rel string
	var err error
	if activePlan == nil {
		key, rel, err = w.canonical(path)
	} else {
		key, rel, err = w.toolCanonical(path, *activePlan)
	}
	if err != nil {
		return err
	}
	if rel == "." {
		return errors.New("cannot modify workspace directory")
	}
	release, err := lockPath(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := os.OpenRoot(w.Root)
	if err != nil {
		return err
	}
	defer r.Close()
	old, readErr := readRegular(ctx, r, rel)
	existed := readErr == nil
	if readErr != nil && (!errors.Is(readErr, os.ErrNotExist) || required) {
		return readErr
	}
	if existed {
		w.mu.RLock()
		prior, ok := w.reads[key]
		w.mu.RUnlock()
		if !ok || prior != bytesDigest(old) {
			return ErrStaleRead
		}
	}
	updated, err := change(old)
	if err != nil {
		return err
	}
	if len(updated) > MaxFileBytes {
		return fmt.Errorf("file exceeds %d-byte write limit", MaxFileBytes)
	}
	parent := filepath.Dir(rel)
	if err := makeDirectoryPath(r, parent, 0755); err != nil {
		return err
	}
	dir, err := openDirectoryRoot(r, parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	name := filepath.Base(rel)
	mode := os.FileMode(0644)
	if existed {
		st, err := dir.Stat(name)
		if err != nil {
			return err
		}
		mode = st.Mode().Perm()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".gopherharness-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := dir.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer dir.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(updated)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Recheck at the commit boundary. The global path lock coordinates our
	// own writers only; it cannot exclude an editor that ignores this protocol.
	current, currentErr := readRegular(ctx, dir, name)
	if existed {
		if currentErr != nil {
			return currentErr
		}
		if bytesDigest(current) != bytesDigest(old) {
			return ErrStaleRead
		}
	} else if currentErr == nil {
		return ErrStaleRead
	} else if !errors.Is(currentErr, os.ErrNotExist) {
		return currentErr
	}
	if err := dir.Rename(tmp, name); err != nil {
		return err
	}
	w.mu.Lock()
	delete(w.reads, key)
	w.mu.Unlock()
	// Directory sync may not be supported on every platform. A failure is
	// returned after rename; callers must inspect the destination before retry.
	return syncRootDirectory(dir)
}
