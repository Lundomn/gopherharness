package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type DreamState struct {
	LastRun      time.Time `json:"last_run,omitempty"`
	LastAttempt  time.Time `json:"last_attempt,omitempty"`
	SessionCount int       `json:"session_count"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
}

func (m *Manager) LastDreamAt() time.Time {
	var state DreamState
	b, err := os.ReadFile(filepath.Join(m.root, ".pico", "memory", "dream-state.json"))
	if err != nil || json.Unmarshal(b, &state) != nil || state.Status != "finished" {
		return time.Time{}
	}
	return state.LastRun
}

func (m *Manager) DreamDue(sessionCount, minSessions int, interval time.Duration) (bool, string) {
	if sessionCount < minSessions {
		return false, "session_gate"
	}
	lastRun := m.LastDreamAt()
	if !lastRun.IsZero() && interval > 0 && time.Since(lastRun) < interval {
		return false, "interval_gate"
	}
	return true, ""
}

func (m *Manager) AcquireDream() (func(), error) {
	path := filepath.Join(m.root, ".pico", "memory", ".consolidate-lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			if !staleDreamLock(path) {
				return nil, fmt.Errorf("dream lock is already held")
			}
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return nil, removeErr
			}
			f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
		if err != nil {
			return nil, err
		}
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close()
	return func() { _ = os.Remove(path) }, nil
}

func (m *Manager) FinishDream(content string, sessionCount int) (string, error) {
	content = sanitizeDream(strings.TrimSpace(content))
	if content == "" {
		return "", fmt.Errorf("dream produced no durable content")
	}
	dir := filepath.Join(m.root, ".pico", "memory", "topics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "dream.md")
	section := fmt.Sprintf("\n## %s\n%s\n", time.Now().UTC().Format(time.RFC3339), content)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(section); err != nil {
		_ = f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	state := DreamState{LastRun: now, LastAttempt: now, SessionCount: sessionCount, Status: "finished"}
	b, _ := json.MarshalIndent(state, "", "  ")
	if err = os.WriteFile(filepath.Join(m.root, ".pico", "memory", "dream-state.json"), append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(".pico", "memory", "topics", "dream.md")), nil
}

func (m *Manager) RecordDreamFailure(sessionCount int, dreamErr error) {
	state := DreamState{LastAttempt: time.Now().UTC(), SessionCount: sessionCount, Status: "failed", Error: dreamErr.Error()}
	b, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(filepath.Join(m.root, ".pico", "memory", "dream-state.json"), append(b, '\n'), 0o600)
}

func staleDreamLock(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	if time.Since(info.ModTime()) > 6*time.Hour {
		return true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var pid int
	if _, err = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil || pid <= 0 {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return process.Signal(syscall.Signal(0)) != nil
}

func sanitizeDream(value string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)sk-[a-z0-9_-]{16,}`),
		regexp.MustCompile(`(?i)(api[_ -]?key\s*[:=]\s*)\S+`),
		regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)\S+`),
		regexp.MustCompile(`(?i)(password|passwd|secret|access[_ -]?token|refresh[_ -]?token)\s*[:=]\s*\S+`),
	}
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}
