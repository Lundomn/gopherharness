package permission

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
)

var (
	ErrApprovalState    = errors.New("approval state does not allow operation")
	ErrApprovalIdentity = errors.New("approval identity mismatch")
	ErrApprovalChanged  = errors.New("approved action changed")
	ErrApprovalExpired  = errors.New("approval expired")
)

// Action is supplied after normalization, validation and path/version checks.
// The caller must recheck authorization and these preconditions before Claim.
// Args are private execution data; this file is 0600, not an audit log.
type Action struct {
	Principal, Session, Turn, CallID, Tool string
	Args                                   map[string]any
	DefinitionDigest                       string
	Preconditions                          map[string]string
}

// Preserve integers larger than 2^53 across durable JSON round trips; changing
// an approved numeric argument while decoding would invalidate the frozen action.
func (a *Action) UnmarshalJSON(data []byte) error {
	type plain Action
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode((*plain)(a))
}

type ApprovalRecord struct {
	ID, Digest, Status              string
	Action                          Action
	CreatedAt, ExpiresAt, UpdatedAt time.Time
	ClaimToken                      string `json:"claim_token,omitempty"`
	Receipt                         string `json:"receipt,omitempty"`
}
type ApprovalState struct{ Records map[string]ApprovalRecord }
type ApprovalStore struct {
	File durable.File[ApprovalState]
	Now  func() time.Time
}
type Claim struct {
	Record ApprovalRecord
	Token  string
}

func OpenApprovals(root string) *ApprovalStore {
	return &ApprovalStore{File: durable.File[ApprovalState]{Path: filepath.Join(root, ".pico", "approvals", "requests.json")}, Now: time.Now}
}
func (s *ApprovalStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func actionDigest(action Action) (string, error) {
	data, err := json.Marshal(action)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
func approvalID(action Action) string {
	data, _ := json.Marshal([]string{action.Principal, action.Session, action.Turn, action.CallID})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Find looks up the immutable execution identity before re-evaluating file
// preconditions. A completed receipt remains usable after its own write changed
// the target; it must not be dispatched again merely to reproduce that result.
func (s *ApprovalStore) Find(ctx context.Context, identity Action) (ApprovalRecord, bool, error) {
	state, err := s.File.Read(ctx)
	if err != nil {
		return ApprovalRecord{}, false, err
	}
	r, ok := state.Records[approvalID(identity)]
	return r, ok, nil
}

// Create is idempotent by trusted principal/session/turn/call identity. Reusing
// that identity with changed arguments is rejected instead of refreshing consent.
func (s *ApprovalStore) Create(ctx context.Context, action Action, ttl time.Duration) (ApprovalRecord, error) {
	if action.Principal == "" || action.Session == "" || action.Turn == "" || action.CallID == "" || action.Tool == "" {
		return ApprovalRecord{}, errors.New("complete approval identity required")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	digest, err := actionDigest(action)
	if err != nil {
		return ApprovalRecord{}, err
	}
	// Freeze an independent copy, not maps that a caller can later mutate.
	data, _ := json.Marshal(action)
	var frozen Action
	if err = json.Unmarshal(data, &frozen); err != nil {
		return ApprovalRecord{}, err
	}
	var record ApprovalRecord
	err = s.File.Update(ctx, func(state *ApprovalState) error {
		if state.Records == nil {
			state.Records = map[string]ApprovalRecord{}
		}
		id := approvalID(action)
		if old, ok := state.Records[id]; ok {
			if old.Digest != digest {
				return ErrApprovalChanged
			}
			record = old
			return nil
		}
		now := s.now()
		record = ApprovalRecord{ID: id, Digest: digest, Status: "pending", Action: frozen, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(ttl)}
		state.Records[id] = record
		return nil
	})
	return record, err
}

func (s *ApprovalStore) Decide(ctx context.Context, id, principal string, allow bool) (ApprovalRecord, error) {
	var record ApprovalRecord
	var resultErr error
	err := s.File.Update(ctx, func(state *ApprovalState) error {
		r, ok := state.Records[id]
		if !ok {
			return ErrApprovalState
		}
		if r.Action.Principal != principal {
			return ErrApprovalIdentity
		}
		if r.Status == "pending" && !s.now().Before(r.ExpiresAt) {
			r.Status = "expired"
			r.UpdatedAt = s.now()
			state.Records[id] = r
			record = r
			resultErr = ErrApprovalExpired
			return nil
		}
		wanted := "denied"
		if allow {
			wanted = "approved"
		}
		if r.Status == wanted {
			record = r
			return nil
		}
		if r.Status != "pending" && !(r.Status == "approved" && !allow) {
			return ErrApprovalState
		}
		r.Status = wanted
		r.UpdatedAt = s.now()
		state.Records[id] = r
		record = r
		return nil
	})
	return record, errors.Join(err, resultErr)
}

// Claim is a durable one-way transition before dispatch. A claimed action is
// NEVER automatically made approved again, including after process restart.
// This avoids duplicate local dispatch; it does not guarantee a remote side
// effect completed exactly once.
func (s *ApprovalStore) Claim(ctx context.Context, id, principal string, current Action) (Claim, error) {
	digest, err := actionDigest(current)
	if err != nil {
		return Claim{}, err
	}
	var claim Claim
	var resultErr error
	err = s.File.Update(ctx, func(state *ApprovalState) error {
		r, ok := state.Records[id]
		if !ok {
			return ErrApprovalState
		}
		if r.Action.Principal != principal || current.Principal != principal {
			return ErrApprovalIdentity
		}
		if r.Digest != digest {
			return ErrApprovalChanged
		}
		if r.Status != "approved" {
			return ErrApprovalState
		}
		if !s.now().Before(r.ExpiresAt) {
			r.Status = "expired"
			r.UpdatedAt = s.now()
			state.Records[id] = r
			resultErr = ErrApprovalExpired
			return nil
		}
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		r.Status = "claimed"
		r.ClaimToken = hex.EncodeToString(token[:])
		r.UpdatedAt = s.now()
		state.Records[id] = r
		claim = Claim{Record: r, Token: r.ClaimToken}
		return nil
	})
	return claim, errors.Join(err, resultErr)
}

// Finish records a receipt under the claim token. "unknown" is mandatory when
// timeout/cancellation might have left a side effect without a reliable receipt.
func (s *ApprovalStore) Finish(ctx context.Context, claim Claim, outcome, receipt string) error {
	if outcome != "completed" && outcome != "failed" && outcome != "unknown" {
		return fmt.Errorf("invalid approval outcome %q", outcome)
	}
	return s.File.Update(ctx, func(state *ApprovalState) error {
		r, ok := state.Records[claim.Record.ID]
		if !ok {
			return ErrApprovalState
		}
		if claim.Token == "" || r.ClaimToken != claim.Token {
			return ErrApprovalIdentity
		}
		if r.Status == outcome && r.Receipt == receipt {
			return nil
		}
		if r.Status != "claimed" {
			return ErrApprovalState
		}
		r.Status = outcome
		r.Receipt = receipt
		r.UpdatedAt = s.now()
		state.Records[r.ID] = r
		return nil
	})
}

func (s *ApprovalStore) Get(ctx context.Context, id, principal string) (ApprovalRecord, error) {
	state, err := s.File.Read(ctx)
	if err != nil {
		return ApprovalRecord{}, err
	}
	r, ok := state.Records[id]
	if !ok {
		return ApprovalRecord{}, ErrApprovalState
	}
	if r.Action.Principal != principal {
		return ApprovalRecord{}, ErrApprovalIdentity
	}
	return r, nil
}

// Pending lists only unclaimed actions for a session. Claimed/unknown actions
// require external outcome reconciliation and are deliberately not resumable.
func (s *ApprovalStore) Pending(ctx context.Context, principal, session string) ([]ApprovalRecord, error) {
	state, err := s.File.Read(ctx)
	if err != nil {
		return nil, err
	}
	var out []ApprovalRecord
	for _, r := range state.Records {
		if r.Action.Principal == principal && r.Action.Session == session && (r.Status == "pending" || r.Status == "approved") && s.now().Before(r.ExpiresAt) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
