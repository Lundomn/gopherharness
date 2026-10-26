package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrControlPath = errors.New("workspace control state is private; generic file tools cannot access .pico")

func controlRelative(rel string) bool {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return strings.EqualFold(first, ".pico")
}
func (w *Workspace) CheckToolPath(path string) error {
	_, _, err := w.toolCanonical(path, "")
	return err
}

// The only control-tree exception is the exact plan file already active for
// this Agent. Aliases resolving to budgets/sessions or another plan are denied.
func (w *Workspace) CheckToolPathForPlan(path, activePlan string) error {
	_, _, err := w.toolCanonical(path, activePlan)
	return err
}
func (w *Workspace) toolCanonical(path, activePlan string) (string, string, error) {
	key, rel, err := w.canonical(path)
	if err != nil {
		return "", "", err
	}
	if !controlRelative(rel) {
		return key, rel, nil
	}
	if activePlan == "" {
		return "", "", ErrControlPath
	}
	plan, planRel, err := w.canonical(activePlan)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(filepath.ToSlash(planRel), "/")
	if len(parts) < 3 || parts[0] != ".pico" || parts[1] != "plans" || key != plan {
		return "", "", ErrControlPath
	}
	return key, rel, nil
}

// Pin a non-symlink parent descriptor first, then verify OpenRoot refers to the
// same directory. This prevents a concurrent directory->symlink replacement
// from redirecting a file write into a private control subtree.
func openDirectoryRoot(root *os.Root, parent string) (*os.Root, error) {
	f, err := openForRead(root, parent)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errors.New("parent is not a directory")
	}
	dir, err := root.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		dir.Close()
		return nil, errors.New("parent changed while opening workspace directory")
	}
	return dir, nil
}
