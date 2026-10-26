package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/evidence"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/protocol"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

func (a *Agent) scheduleDream(origin context.Context, recorder *evidence.Recorder) {
	count := a.ownSessionCount(a.Memory.LastDreamAt(), a.Session.ID)
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
	lifetime := a.lifecycleCtx
	if lifetime == nil {
		lifetime = context.Background()
	}
	// Maintenance inherits the causal trace, but cancellation belongs to the
	// owning Agent. Finishing the foreground turn must not kill accepted work.
	lifetime = tracing.Attach(lifetime, tracing.Current(origin))
	go func() {
		defer a.dreamWG.Done()
		defer release()
		ctx, cancel := context.WithTimeout(lifetime, 5*time.Minute)
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if a.lifecycleCtx != nil {
		stop := context.AfterFunc(a.lifecycleCtx, cancel)
		defer stop()
	}
	if err := a.mu.LockContext(ctx); err != nil {
		return "", err
	}
	defer a.mu.Unlock()
	if a.closed.Load() {
		return "", fmt.Errorf("agent is closed")
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer timeoutCancel()
	release, err := a.Memory.AcquireDream()
	if err != nil {
		return "", err
	}
	defer release()
	path, err := a.consolidateDream(ctx, a.ownSessionCount(time.Time{}, ""))
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
	if err := ctx.Err(); err != nil {
		return "", err
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
	// Filter by trusted session owner before applying the recent-session limit.
	var own []string
	for _, p := range paths {
		s, err := a.Sessions.Load(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err == nil && a.ownsMemorySession(s) {
			own = append(own, p)
		}
	}
	paths = own
	if len(paths) > limit {
		paths = paths[len(paths)-limit:]
	}
	var b strings.Builder
	for _, path := range paths {
		s, readErr := a.Sessions.Load(strings.TrimSuffix(filepath.Base(path), ".json"))
		if readErr != nil || !a.ownsMemorySession(s) {
			continue
		}
		// Opaque provider reasoning/signatures and internal state are not facts.
		messages := make([]map[string]string, 0, len(s.Messages))
		for _, m := range s.Messages {
			messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
		}
		data, _ := json.Marshal(messages)
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

func (a *Agent) ownsMemorySession(s *model.Session) bool {
	user := s.MemoryUser
	if user == "" {
		user = "local"
	}
	return user == a.FactScope.User
}
func (a *Agent) ownSessionCount(since time.Time, exclude string) int {
	entries, err := os.ReadDir(a.Sessions.Dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		s, err := a.Sessions.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err == nil && s.ID != exclude && s.UpdatedAt.After(since) && a.ownsMemorySession(s) {
			n++
		}
	}
	return n
}
