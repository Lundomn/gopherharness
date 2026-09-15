package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/model"
)

type SessionStore struct{ Dir string }

func NewSessionStore(root string) *SessionStore {
	return &SessionStore{Dir: filepath.Join(root, ".pico", "sessions")}
}

func (s *SessionStore) New(workspace string) *model.Session {
	now := time.Now().UTC()
	return &model.Session{SchemaVersion: model.SessionSchema, ID: NewID("session"), Workspace: workspace, Messages: []model.Message{}, Memory: map[string]any{}, CreatedAt: now, UpdatedAt: now}
}

func (s *SessionStore) Save(session *model.Session) error {
	session.UpdatedAt = time.Now().UTC()
	return writeJSON(filepath.Join(s.Dir, session.ID+".json"), session)
}

func (s *SessionStore) Load(id string) (*model.Session, error) {
	if id == "latest" {
		var err error
		id, err = s.Latest()
		if err != nil {
			return nil, err
		}
	}
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return nil, fmt.Errorf("invalid session id")
	}
	var session model.Session
	if err := readJSON(filepath.Join(s.Dir, id+".json"), &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *SessionStore) Latest() (string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no sessions")
	}
	sort.Strings(names)
	return names[len(names)-1][:len(names[len(names)-1])-5], nil
}

func (s *SessionStore) AppendEvent(sessionID string, event model.TraceEvent) error {
	event.SchemaVersion = model.EventSchema
	event.SessionID = sessionID
	return appendJSONL(filepath.Join(s.Dir, sessionID+".events.jsonl"), event)
}

func (s *SessionStore) Count() int {
	return s.CountSince(time.Time{}, "")
}

func (s *SessionStore) CountSince(since time.Time, excludeID string) int {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || strings.HasSuffix(entry.Name(), ".events.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if id == excludeID {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().After(since) {
			count++
		}
	}
	return count
}

func LoadLatestCheckpoint(root, sessionID string) (*model.Checkpoint, error) {
	runsDir := filepath.Join(root, ".pico", "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, err
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, entry.Name())
		var report model.RunReport
		if readJSON(filepath.Join(dir, "report.json"), &report) != nil || report.SessionID != sessionID {
			continue
		}
		var checkpoint model.Checkpoint
		if readJSON(filepath.Join(dir, "checkpoint.json"), &checkpoint) == nil {
			return &checkpoint, nil
		}
	}
	return nil, os.ErrNotExist
}

type RunStore struct{ Root, RunID, Dir string }

func NewRunStore(root string) *RunStore {
	id := NewID("run")
	return &RunStore{Root: root, RunID: id, Dir: filepath.Join(root, ".pico", "runs", id)}
}
func (r *RunStore) WriteTask(v *model.TaskState) error {
	return writeJSON(filepath.Join(r.Dir, "task_state.json"), v)
}
func (r *RunStore) WriteReport(v *model.RunReport) error {
	return writeJSON(filepath.Join(r.Dir, "report.json"), v)
}
func (r *RunStore) AppendTrace(v model.TraceEvent) error {
	v.SchemaVersion = model.EventSchema
	v.RunID = r.RunID
	return appendJSONL(filepath.Join(r.Dir, "trace.jsonl"), v)
}
func (r *RunStore) WriteCheckpoint(v model.Checkpoint) error {
	return writeJSON(filepath.Join(r.Dir, "checkpoint.json"), v)
}
func (r *RunStore) WriteArtifact(name string, data []byte) (string, error) {
	path := filepath.Join(r.Dir, "artifacts", filepath.Base(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o600)
}

func NewID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s-%s", prefix, time.Now().UTC().Format("20060102-150405.000000"), hex.EncodeToString(b))
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp-" + hex.EncodeToString([]byte(fmt.Sprint(time.Now().UnixNano())))
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func readJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
func appendJSONL(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if _, err = w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}
