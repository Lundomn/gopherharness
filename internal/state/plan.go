package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/Lundomn/gopherharness/internal/workspace"
)

type PlanController struct {
	root   string
	mu     sync.RWMutex
	active bool
	path   string
}

func NewPlan(root string) *PlanController { return &PlanController{root: root} }
func (p *PlanController) Enter(rel string) (string, error) {
	if rel == "" {
		rel = ".pico/plans/active.md"
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("plan path must be workspace-relative")
	}
	w, err := workspace.Open(p.root)
	if err != nil {
		return "", err
	}
	path, err := w.Resolve(rel)
	if err != nil {
		return "", err
	}
	clean := w.Relative(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err = os.WriteFile(path, []byte("# Active plan\n\n"), 0o600); err != nil {
			return "", err
		}
	}
	p.mu.Lock()
	p.active = true
	p.path = clean
	p.mu.Unlock()
	return "plan mode active: " + clean, nil
}
func (p *PlanController) Exit() string {
	p.mu.Lock()
	p.active = false
	path := p.path
	p.mu.Unlock()
	if path == "" {
		return "plan mode inactive"
	}
	return "plan mode exited: " + path
}
func (p *PlanController) Active() (bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.active, p.path
}
