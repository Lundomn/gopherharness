package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

func TestSessionAndRunArtifacts(t *testing.T) {
	root := t.TempDir()
	ss := NewSessionStore(root)
	s := ss.New(root)
	s.Messages = append(s.Messages, model.Message{Role: "user", Content: "hi", CreatedAt: time.Now()})
	if err := ss.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, err := ss.Load("latest")
	if err != nil || loaded.ID != s.ID {
		t.Fatalf("load=%#v err=%v", loaded, err)
	}
	rs := NewRunStore(root)
	task := &model.TaskState{SchemaVersion: model.ArtifactSchema, RunID: rs.RunID}
	if err = rs.WriteTask(task); err != nil {
		t.Fatal(err)
	}
	if err = rs.AppendTrace(model.TraceEvent{Type: "run_started", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"task_state.json", "trace.jsonl"} {
		if _, err = os.Stat(filepath.Join(rs.Dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionLoadRejectsPathTraversal(t *testing.T) {
	ss := NewSessionStore(t.TempDir())
	for _, id := range []string{"../outside", "/tmp/outside", "..", "."} {
		if _, err := ss.Load(id); err == nil {
			t.Fatalf("session id %q was accepted", id)
		}
	}
}
