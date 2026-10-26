package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
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
	b, err := os.ReadFile(filepath.Join(m.memoryDir(), "dream-state.json"))
	if err != nil || json.Unmarshal(b, &state) != nil {
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
	path := filepath.Join(m.memoryDir(), ".consolidate-lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	release, err := durable.Lease(ctx, path)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("dream lock is already held")
	}
	// Do not delete lock files or steal by mtime/PID: the kernel releases the
	// lock on process death and a live owner may legitimately be slow.
	return release, err
}

func (m *Manager) FinishDream(content string, sessionCount int) (string, error) {
	content = sanitizeDream(strings.TrimSpace(content))
	if content == "" {
		return "", fmt.Errorf("dream produced no durable content")
	}
	dir := filepath.Join(m.memoryDir(), "topics")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "dream.md")
	section := fmt.Sprintf("\n## %s\n%s\n", time.Now().UTC().Format(time.RFC3339), content)
	release, err := durable.Lease(context.Background(), path+".lock")
	if err != nil {
		return "", err
	}
	defer release()
	previous, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err = durable.Replace(path, append(previous, []byte(section)...)); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	state := DreamState{LastRun: now, LastAttempt: now, SessionCount: sessionCount, Status: "finished"}
	b, _ := json.MarshalIndent(state, "", "  ")
	if err = durable.Replace(filepath.Join(m.memoryDir(), "dream-state.json"), append(b, '\n')); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(m.root, path)
	return filepath.ToSlash(rel), err
}

func (m *Manager) RecordDreamFailure(sessionCount int, dreamErr error) {
	f := durable.File[DreamState]{Path: filepath.Join(m.memoryDir(), "dream-state.json")}
	_ = f.Update(context.Background(), func(state *DreamState) error {
		state.LastAttempt = time.Now().UTC()
		state.SessionCount = sessionCount
		state.Status = "failed"
		state.Error = dreamErr.Error()
		return nil
	})
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
