package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/protocol"
)

func (a *Agent) scheduleDream(recorder *evidence.Recorder) {
	count := a.Sessions.CountSince(a.Memory.LastDreamAt(), a.Session.ID)
	if !a.AutoDream {
		_ = recorder.Emit("memory_auto_dream_skipped", map[string]any{"reason": "disabled", "session_count": count})
		return
	}
	if due, reason := a.Memory.DreamDue(count, a.DreamMinSessions, a.DreamInterval); !due {
		_ = recorder.Emit("memory_auto_dream_skipped", map[string]any{"reason": reason, "session_count": count})
		return
	}
	release, err := a.Memory.AcquireDream()
	if err != nil {
		_ = recorder.Emit("memory_auto_dream_skipped", map[string]any{"reason": "lock_held", "session_count": count})
		return
	}
	_ = recorder.Emit("memory_auto_dream_started", map[string]any{"status": "submitted", "session_count": count})
	a.dreamWG.Add(1)
	go func() {
		defer a.dreamWG.Done()
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		path, dreamErr := a.consolidateDream(ctx, count)
		a.dreamErrMu.Lock()
		a.lastDreamErr = dreamErr
		a.dreamErrMu.Unlock()
		if dreamErr != nil {
			a.Memory.RecordDreamFailure(count, dreamErr)
			_ = recorder.Emit("memory_auto_dream_failed", map[string]any{"error": dreamErr.Error(), "session_count": count})
			return
		}
		_ = recorder.Emit("memory_auto_dream_finished", map[string]any{"status": "finished", "session_count": count, "changed_files": []string{path}})
	}()
}

func (a *Agent) RunDream(ctx context.Context) (string, error) {
	release, err := a.Memory.AcquireDream()
	if err != nil {
		return "", err
	}
	defer release()
	path, err := a.consolidateDream(ctx, a.Sessions.Count())
	if err != nil {
		return "", err
	}
	return "Dream consolidation complete: " + path, nil
}

func (a *Agent) WaitForMemoryMaintenance() error {
	a.dreamWG.Wait()
	a.dreamErrMu.Lock()
	defer a.dreamErrMu.Unlock()
	return a.lastDreamErr
}

func (a *Agent) consolidateDream(ctx context.Context, sessionCount int) (string, error) {
	prompt := "Dream: Memory Consolidation\n\nExtract only durable project facts, decisions, conventions, dependencies, and unresolved blockers. Merge duplicates. Reject credentials, secrets, transient chatter, and relative-date ambiguity. Return concise Markdown only; do not call tools.\n\n" + a.Memory.Render("") + "\n\nRecent session transcripts:\n" + a.recentSessionText(5, 24000)
	completion, err := a.Provider.Complete(ctx, prompt, min(a.MaxNewTokens, 4096))
	if err != nil {
		return "", fmt.Errorf("dream provider: %w", err)
	}
	content := completion.Text
	if parsed := protocol.Parse(content); parsed.Kind == protocol.KindFinal {
		content = parsed.Text
	}
	return a.Memory.FinishDream(content, sessionCount)
}

func (a *Agent) recentSessionText(limit, maxChars int) string {
	entries, err := os.ReadDir(a.Sessions.Dir)
	if err != nil {
		return "(none)"
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			paths = append(paths, filepath.Join(a.Sessions.Dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) > limit {
		paths = paths[len(paths)-limit:]
	}
	var b strings.Builder
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		remaining := maxChars - b.Len()
		if remaining <= 0 {
			break
		}
		if len(data) > remaining {
			data = data[:remaining]
		}
		fmt.Fprintf(&b, "\n# %s\n%s", filepath.Base(path), data)
	}
	if b.Len() == 0 {
		return "(none)"
	}
	return b.String()
}
