package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Lundomn/gopherharness/internal/redact"
	"github.com/Lundomn/gopherharness/internal/store"
)

func TestRecorderWritesRedactedRunAndSessionEvents(t *testing.T) {
	root := t.TempDir()
	run := store.NewRunStore(root)
	sessions := store.NewSessionStore(root)
	recorder := New(run, sessions, "session-test", redact.New("provider-secret"))
	if err := recorder.Emit("tool_started", map[string]any{"args": map[string]any{"api_key": "provider-secret", "command": "echo sk-abcdefghijklmnop"}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(run.Dir, "trace.jsonl"), filepath.Join(sessions.Dir, "session-test.events.jsonl")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "provider-secret") || strings.Contains(string(data), "sk-abcdefghijklmnop") || !strings.Contains(string(data), redact.Replacement) {
			t.Fatalf("event was not redacted: %s", data)
		}
	}
}
