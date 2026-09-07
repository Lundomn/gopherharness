package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Todo struct {
	ID, Text, Status     string
	CreatedAt, UpdatedAt time.Time
}
type TodoLedger struct {
	path  string
	mu    sync.RWMutex
	Items []Todo
}

func OpenTodo(root string) *TodoLedger {
	l := &TodoLedger{path: filepath.Join(root, ".pico", "todos.json")}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, &l.Items)
	}
	return l
}
func (l *TodoLedger) Add(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "error: todo text is required"
	}
	l.mu.Lock()
	id := fmt.Sprintf("todo-%03d", len(l.Items)+1)
	now := time.Now().UTC()
	l.Items = append(l.Items, Todo{ID: id, Text: text, Status: "pending", CreatedAt: now, UpdatedAt: now})
	l.mu.Unlock()
	_ = l.Save()
	return id
}
func (l *TodoLedger) Update(id, status string) error {
	valid := map[string]bool{"pending": true, "in_progress": true, "done": true, "blocked": true}
	if !valid[status] {
		return fmt.Errorf("invalid todo status %q", status)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.Items {
		if l.Items[i].ID == id {
			l.Items[i].Status = status
			l.Items[i].UpdatedAt = time.Now().UTC()
			return l.saveLocked()
		}
	}
	return fmt.Errorf("todo %q not found", id)
}
func (l *TodoLedger) List() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if len(l.Items) == 0 {
		return "(no todos)"
	}
	var b strings.Builder
	for _, t := range l.Items {
		fmt.Fprintf(&b, "- [%s] %s: %s\n", t.Status, t.ID, t.Text)
	}
	return strings.TrimSpace(b.String())
}
func (l *TodoLedger) Save() error { l.mu.RLock(); defer l.mu.RUnlock(); return l.saveLocked() }
func (l *TodoLedger) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l.Items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path, append(b, '\n'), 0o600)
}
