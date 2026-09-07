package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type FileSummary struct {
	Path, Summary, Digest string
	UpdatedAt             time.Time
}
type Note struct {
	Text, Kind, Source string
	CreatedAt          time.Time
}
type State struct {
	CurrentGoal   string                 `json:"current_goal,omitempty"`
	Blockers      []string               `json:"blockers,omitempty"`
	NextStep      string                 `json:"next_step,omitempty"`
	RecentFiles   []string               `json:"recent_files,omitempty"`
	FileSummaries map[string]FileSummary `json:"file_summaries,omitempty"`
	Notes         []Note                 `json:"notes,omitempty"`
}

type Manager struct {
	root, path string
	mu         sync.RWMutex
	state      State
}

func Open(root string) (*Manager, error) {
	m := &Manager{root: root, path: filepath.Join(root, ".pico", "memory", "working.json"), state: State{FileSummaries: map[string]FileSummary{}}}
	if b, err := os.ReadFile(m.path); err == nil {
		if err = json.Unmarshal(b, &m.state); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if m.state.FileSummaries == nil {
		m.state.FileSummaries = map[string]FileSummary{}
	}
	return m, nil
}
func (m *Manager) Snapshot() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, _ := json.Marshal(m.state)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *Manager) SetGoal(goal string) {
	m.mu.Lock()
	m.state.CurrentGoal = sanitizeDream(goal)
	m.mu.Unlock()
	_ = m.Save()
}
func (m *Manager) SetNextStep(step string) {
	m.mu.Lock()
	m.state.NextStep = step
	m.mu.Unlock()
	_ = m.Save()
}
func (m *Manager) AddNote(text, kind, source string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	sanitized := sanitizeDream(text)
	if sanitized != text {
		return
	}
	m.mu.Lock()
	m.state.Notes = append(m.state.Notes, Note{Text: text, Kind: kind, Source: source, CreatedAt: time.Now().UTC()})
	if len(m.state.Notes) > 100 {
		m.state.Notes = m.state.Notes[len(m.state.Notes)-100:]
	}
	m.mu.Unlock()
	_ = m.Save()
}
func (m *Manager) ObserveFile(rel, summary string) {
	summary = sanitizeDream(summary)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.RecentFiles = prependUnique(m.state.RecentFiles, rel, 12)
	m.state.FileSummaries[rel] = FileSummary{Path: rel, Summary: clip(strings.TrimSpace(summary), 800), Digest: digest(summary), UpdatedAt: time.Now().UTC()}
	_ = m.saveLocked()
}
func (m *Manager) Invalidate(rel string) {
	m.mu.Lock()
	delete(m.state.FileSummaries, rel)
	m.mu.Unlock()
	_ = m.Save()
}
func (m *Manager) Save() error { m.mu.RLock(); defer m.mu.RUnlock(); return m.saveLocked() }
func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(b, '\n'), 0o600)
}

func (m *Manager) Render(query string) string {
	s := m.Snapshot()
	var b strings.Builder
	b.WriteString("Working memory:\n")
	fmt.Fprintf(&b, "- current goal: %s\n", empty(s.CurrentGoal))
	fmt.Fprintf(&b, "- next step: %s\n", empty(s.NextStep))
	fmt.Fprintf(&b, "- blockers: %s\n", joinOrNone(s.Blockers))
	fmt.Fprintf(&b, "- recent files: %s\n", joinOrNone(s.RecentFiles))
	for _, p := range s.RecentFiles {
		if v, ok := s.FileSummaries[p]; ok {
			fmt.Fprintf(&b, "- file %s: %s\n", p, v.Summary)
		}
	}
	for _, n := range relevantNotes(s.Notes, query, 5) {
		fmt.Fprintf(&b, "- note[%s]: %s\n", n.Kind, n.Text)
	}
	return strings.TrimSpace(b.String())
}

func (m *Manager) Promote(final string) error {
	lines := strings.Split(final, "\n")
	var durable []string
	re := regexp.MustCompile(`(?i)^\s*(decision|project convention|dependency|约定|决策|依赖)\s*[:：]\s*(.+)$`)
	for _, line := range lines {
		if match := re.FindStringSubmatch(strings.TrimSpace(strings.TrimPrefix(line, "-"))); len(match) == 3 {
			fact := sanitizeDream(match[2])
			if fact == match[2] {
				durable = append(durable, "- "+match[1]+": "+fact)
			}
		}
	}
	if len(durable) == 0 {
		return nil
	}
	dir := filepath.Join(m.root, ".pico", "memory", "topics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "project.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n## %s\n%s\n", time.Now().UTC().Format(time.RFC3339), strings.Join(durable, "\n"))
	return err
}

func relevantNotes(notes []Note, query string, limit int) []Note {
	tokens := tokenize(query)
	type scored struct {
		n Note
		s int
	}
	var rows []scored
	for _, n := range notes {
		s := 0
		lower := strings.ToLower(n.Text)
		for _, t := range tokens {
			if strings.Contains(lower, t) {
				s++
			}
		}
		if s > 0 {
			rows = append(rows, scored{n, s})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].s > rows[j].s })
	var out []Note
	for i := 0; i < len(rows) && i < limit; i++ {
		out = append(out, rows[i].n)
	}
	return out
}
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127) })
	var out []string
	for _, f := range fields {
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
func prependUnique(items []string, value string, limit int) []string {
	out := []string{value}
	for _, v := range items {
		if v != value {
			out = append(out, v)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func empty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}
func joinOrNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}
