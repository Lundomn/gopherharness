// Package journal owns durable inbound requests and outbound results. Running
// work interrupted by process death is reconciled, never blindly replayed.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
)

type Entry struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	State     string          `json:"state"`
	Payload   json.RawMessage `json:"payload"`
	Result    json.RawMessage `json:"result,omitempty"`
	Epoch     uint64          `json:"epoch"`
	UpdatedAt time.Time       `json:"updated_at"`
}
type State struct {
	Epoch   uint64
	Entries map[string]Entry
}
type Journal struct {
	File      durable.File[State]
	epoch     uint64
	release   func()
	closeOnce sync.Once
}

var ErrFence = errors.New("journal owner epoch changed")

func Open(ctx context.Context, root string) (*Journal, error) {
	path := filepath.Join(root, ".pico", "gateway", "journal.json")
	release, err := durable.Lease(ctx, path+".owner")
	if err != nil {
		return nil, err
	}
	j := &Journal{File: durable.File[State]{Path: path}, release: release}
	err = j.File.Update(ctx, func(s *State) error {
		s.Epoch++
		j.epoch = s.Epoch
		if s.Entries == nil {
			s.Entries = map[string]Entry{}
		}
		for key, e := range s.Entries {
			switch e.State {
			case "running":
				e.State = "needs_reconcile"
			case "sending":
				e.State = "unknown"
			}
			e.Epoch = s.Epoch
			s.Entries[key] = e
		}
		return nil
	})
	if err != nil {
		release()
		return nil, err
	}
	return j, nil
}
func (j *Journal) mutate(ctx context.Context, fn func(*State) error) error {
	return j.File.Update(ctx, func(s *State) error {
		if s.Epoch != j.epoch {
			return ErrFence
		}
		return fn(s)
	})
}
func key(kind, id string) string { return kind + ":" + id }
func (j *Journal) Put(ctx context.Context, kind, id string, payload any) (bool, error) {
	if id == "" || kind == "" {
		return false, errors.New("journal identity required")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	created := false
	err = j.mutate(ctx, func(s *State) error {
		if _, ok := s.Entries[key(kind, id)]; ok {
			return nil
		}
		s.Entries[key(kind, id)] = Entry{ID: id, Kind: kind, State: "pending", Payload: data, Epoch: j.epoch, UpdatedAt: time.Now().UTC()}
		created = true
		return nil
	})
	return created, err
}
func (j *Journal) Move(ctx context.Context, kind, id, from, to string, result any) error {
	var data []byte
	var err error
	if result != nil {
		data, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	return j.mutate(ctx, func(s *State) error {
		k := key(kind, id)
		e, ok := s.Entries[k]
		if !ok || e.State != from {
			return fmt.Errorf("journal transition rejected: %s %s to %s", k, from, to)
		}
		e.State = to
		e.Result = data
		e.UpdatedAt = time.Now().UTC()
		s.Entries[k] = e
		return nil
	})
}

// Complete atomically publishes the result to Outbox and settles Inbox.
func (j *Journal) Complete(ctx context.Context, id string, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return j.mutate(ctx, func(s *State) error {
		k := key("inbox", id)
		e, ok := s.Entries[k]
		if !ok || e.State != "running" {
			return errors.New("inbound request not owned or not running")
		}
		e.State = "completed"
		e.Result = data
		e.UpdatedAt = time.Now().UTC()
		s.Entries[k] = e
		s.Entries[key("outbox", id)] = Entry{ID: id, Kind: "outbox", State: "pending", Payload: data, Epoch: j.epoch, UpdatedAt: e.UpdatedAt}
		return nil
	})
}
func (j *Journal) List(ctx context.Context, kind, state string) ([]Entry, error) {
	s, err := j.File.Read(ctx)
	if err != nil {
		return nil, err
	}
	var result []Entry
	for _, e := range s.Entries {
		if e.Kind == kind && (state == "" || e.State == state) {
			result = append(result, e)
		}
	}
	sort.Slice(result, func(a, b int) bool { return result[a].UpdatedAt.Before(result[b].UpdatedAt) })
	return result, nil
}
func (j *Journal) Close() {
	j.closeOnce.Do(func() {
		if j.release != nil {
			j.release()
		}
	})
}
