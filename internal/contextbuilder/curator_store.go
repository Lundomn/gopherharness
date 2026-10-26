package contextbuilder

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/redact"
)

// The OS root confines all model-provided archive refs. A session hashes to an
// isolated namespace; callers never pass absolute archive paths to the model.
type curatorStore struct {
	root   *os.Root
	redact func(string) string
}

func openCuratorStore(dir, session string) (*curatorStore, error) {
	if session == "" {
		return nil, errors.New("curator archive requires session key")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	hash := sha256.Sum256([]byte(session))
	name := hex.EncodeToString(hash[:])
	if err := root.MkdirAll(name, 0700); err != nil {
		return nil, err
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &curatorStore{root: child}, nil
}
func (s *curatorStore) Close() error { return s.root.Close() }

func loadWorkingState(cfg *CuratorConfig) (string, error) {
	store, err := openCuratorStore(cfg.ArchiveDir, cfg.SessionKey)
	if err != nil {
		return "", err
	}
	defer store.Close()
	state, err := store.readState()
	if err != nil {
		return "", fmt.Errorf("curator state: %w", err)
	}
	if len(state.Goals)+len(state.OpenThreads)+len(state.Decisions) == 0 {
		return "", nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return "# Working State\n" + string(data), nil
}

// Temp files are created exclusively and then atomically replaced. A reference
// is returned only after data and the containing directory have been synced.
func (s *curatorStore) write(name string, content []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := ".tmp-" + hex.EncodeToString(nonce[:])
	f, err := s.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer s.root.Remove(temp)
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = s.root.Rename(temp, name); err != nil {
		return err
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *curatorStore) read(name string) ([]byte, error) {
	f, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxArchive = 64 << 20
	b, err := io.ReadAll(io.LimitReader(f, maxArchive+1))
	if len(b) > maxArchive {
		return nil, errors.New("curator archive too large")
	}
	return b, err
}

func (s *curatorStore) readState() (WorkingState, error) {
	var state WorkingState
	b, err := s.read("working-state.json")
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	return boundedState(state), nil
}
func (s *curatorStore) writeState(state WorkingState) error {
	b, err := s.safeJSON(boundedState(state))
	if err != nil {
		return err
	}
	return s.write("working-state.json", b)
}

type archiveRecord struct {
	GroupID int           `json:"group_id"`
	Message model.Message `json:"message"`
}

var archiveRefPattern = regexp.MustCompile(`^[a-f0-9]{64}\.jsonl$`)

func (s *curatorStore) archive(groups []Group) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, g := range groups {
		for _, m := range g.Messages {
			safe, err := s.safeJSON(archiveRecord{g.ID, m})
			if err != nil {
				return "", err
			}
			if err := enc.Encode(json.RawMessage(safe)); err != nil {
				return "", err
			}
			if b.Len() > 64<<20 {
				return "", errors.New("curator archive exceeds 64 MiB")
			}
		}
	}
	hash := sha256.Sum256(b.Bytes())
	ref := hex.EncodeToString(hash[:]) + ".jsonl"
	if err := s.write(ref, b.Bytes()); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *curatorStore) safeJSON(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	generic = redact.New().Value(generic)
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if s.redact != nil {
				return s.redact(x)
			}
			return x
		case []any:
			for i, c := range x {
				x[i] = walk(c)
			}
			return x
		case map[string]any:
			for k, c := range x {
				x[k] = walk(c)
			}
			return x
		default:
			return v
		}
	}
	return json.Marshal(walk(generic))
}

func (s *curatorStore) retrieve(ref string) ([]Group, error) {
	if !archiveRefPattern.MatchString(ref) {
		return nil, errors.New("invalid archive reference")
	}
	b, err := s.read(ref)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(b)
	if ref != hex.EncodeToString(hash[:])+".jsonl" {
		return nil, errors.New("archive checksum mismatch")
	}
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 4096), 64<<20)
	groups := []Group{}
	byID := map[int]int{}
	for scanner.Scan() {
		var record archiveRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		idx, ok := byID[record.GroupID]
		if !ok {
			idx = len(groups)
			byID[record.GroupID] = idx
			groups = append(groups, Group{ID: record.GroupID})
		}
		groups[idx].Messages = append(groups[idx].Messages, record.Message)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func groupFingerprint(g Group) string {
	b, _ := json.Marshal(g.Messages)
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

type persistedRelevance struct {
	Score      float64 `json:"score"`
	ArchiveRef string  `json:"archive_ref,omitempty"`
}

func (s *curatorStore) loadScores(groups []Group) error {
	b, err := s.read("relevance.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var scores map[string]persistedRelevance
	if err := json.Unmarshal(b, &scores); err != nil {
		return err
	}
	total := 0
	for _, g := range groups {
		total += len(g.Messages)
	}
	offset := 0
	for i := range groups {
		if entry, ok := scores[groupFingerprint(groups[i])]; ok {
			if entry.Score < 0 || entry.Score > 1 || (entry.ArchiveRef != "" && !archiveRefPattern.MatchString(entry.ArchiveRef)) {
				return fmt.Errorf("invalid persisted relevance")
			}
			// New recent/head protection is evaluated on the current history.
			groups[i].Score = entry.Score
			groups[i].ArchiveRef = entry.ArchiveRef
			if offset+len(groups[i].Messages) > total-12 {
				groups[i].Score = max(groups[i].Score, .75)
			}
			if groups[i].Protected {
				groups[i].Score = max(groups[i].Score, .8)
			}
		}
		offset += len(groups[i].Messages)
	}
	return nil
}
func (s *curatorStore) saveScores(groups []Group) error {
	scores := map[string]persistedRelevance{}
	for _, g := range groups {
		scores[groupFingerprint(g)] = persistedRelevance{g.Score, g.ArchiveRef}
	}
	b, err := json.Marshal(scores)
	if err != nil {
		return err
	}
	return s.write("relevance.json", b)
}
