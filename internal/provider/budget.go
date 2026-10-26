package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/tracing"
)

const NanoUSDPerDollar int64 = 1_000_000_000

var ErrBudgetExhausted = errors.New("model cost budget exhausted")
var ErrBudgetAccounting = errors.New("model cost accounting requires reconciliation")
var ErrBudgetState = errors.New("model cost budget persistence failed")
var ErrNestedBudget = errors.New("nested model cost budget wrapper")

// FixedPrice is nano-USD per million normalized tokens. Cache read/write are
// separate token classes, not added again to the fresh input token count.
// Every model must have an explicit entry; a zero rate means explicitly free.
type FixedPrice struct{ Input, Output, CacheRead, CacheWrite int64 }

// MoneyFromUSD converts the decimal representation once at configuration time.
// All subsequent accounting uses integer arithmetic, rounding upward by less
// than one nano-dollar, never binary floating-point balance updates.
func MoneyFromUSD(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("invalid USD amount")
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	if !ok {
		return 0, errors.New("invalid USD amount")
	}
	r.Mul(r, big.NewRat(NanoUSDPerDollar, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("USD amount overflow")
	}
	return q.Int64(), nil
}

func FixedPrices(prices map[string]Price) (map[string]FixedPrice, error) {
	result := map[string]FixedPrice{}
	for model, p := range prices {
		var f FixedPrice
		values := []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite}
		outputs := []*int64{&f.Input, &f.Output, &f.CacheRead, &f.CacheWrite}
		for i, v := range values {
			n, err := MoneyFromUSD(v)
			if err != nil {
				return nil, fmt.Errorf("price for %s: %w", model, err)
			}
			*outputs[i] = n
		}
		result[model] = f
	}
	return result, nil
}

func (p FixedPrice) valid() bool {
	return p.Input >= 0 && p.Output >= 0 && p.CacheRead >= 0 && p.CacheWrite >= 0
}
func (p FixedPrice) cost(u Usage) (int64, error) {
	if !p.valid() || !u.Complete || u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 {
		return 0, ErrBudgetAccounting
	}
	total := new(big.Int)
	for i, rate := range []int64{p.Input, p.Output, p.CacheRead, p.CacheWrite} {
		tokens := []int{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens}[i]
		v := new(big.Int).Mul(big.NewInt(rate), big.NewInt(int64(tokens)))
		total.Add(total, v)
	}
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(total, big.NewInt(1_000_000), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errors.New("token cost overflow")
	}
	return q.Int64(), nil
}

type BudgetAttempt struct {
	ID                  string           `json:"id"`
	State               string           `json:"state"` // reserved, sending, unknown, settled, released
	Provider            string           `json:"provider"`
	Model               string           `json:"model"`
	ActualModel         string           `json:"actual_model,omitempty"`
	ReservedNanoUSD     int64            `json:"reserved_nano_usd"`
	CostNanoUSD         int64            `json:"cost_nano_usd"`
	Price               FixedPrice       `json:"price"`
	Usage               Usage            `json:"usage"`
	Identity            tracing.Identity `json:"identity"`
	Reason              string           `json:"reason,omitempty"`
	NeedsReconciliation bool             `json:"needs_reconciliation,omitempty"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}
type BudgetState struct {
	Schema       string                   `json:"schema"`
	LimitNanoUSD int64                    `json:"limit_nano_usd"`
	Attempts     map[string]BudgetAttempt `json:"attempts"`
}
type BudgetSnapshot struct {
	LimitNanoUSD     int64                    `json:"limit_nano_usd"`
	SpentNanoUSD     int64                    `json:"spent_nano_usd"`
	HeldNanoUSD      int64                    `json:"held_nano_usd"`
	RemainingNanoUSD int64                    `json:"remaining_nano_usd"`
	Frozen           bool                     `json:"frozen"`
	Attempts         map[string]BudgetAttempt `json:"attempts"`
}
type BudgetAccount struct{ file durable.File[BudgetState] }

// OpenBudget never silently increases a persistent limit. Passing a different
// limit is an error; billing windows use a different explicitly chosen file.
func OpenBudget(ctx context.Context, path string, limitNanoUSD int64) (*BudgetAccount, error) {
	if path == "" || limitNanoUSD < 0 {
		return nil, errors.New("invalid budget path or limit")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	exists := false
	if _, statErr := os.Stat(abs); statErr == nil {
		exists = true
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	a := &BudgetAccount{file: durable.File[BudgetState]{Path: abs}}
	err = a.file.Update(ctx, func(s *BudgetState) error {
		if s.Schema == "" {
			if exists || s.Attempts != nil || s.LimitNanoUSD != 0 {
				return errors.New("missing persistent budget schema")
			}
			s.Schema = "gopherharness.budget.v1"
			s.LimitNanoUSD = limitNanoUSD
			s.Attempts = map[string]BudgetAttempt{}
		}
		if s.Schema != "gopherharness.budget.v1" || s.LimitNanoUSD != limitNanoUSD {
			return errors.New("persistent budget schema/limit mismatch")
		}
		_, err := budgetSnapshot(*s)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err = a.Recover(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func budgetSnapshot(s BudgetState) (BudgetSnapshot, error) {
	out := BudgetSnapshot{LimitNanoUSD: s.LimitNanoUSD, Attempts: s.Attempts}
	if s.Schema != "gopherharness.budget.v1" || s.LimitNanoUSD < 0 || s.Attempts == nil {
		return out, errors.New("invalid budget state")
	}
	add := func(target *int64, value int64) error {
		if value < 0 || *target > math.MaxInt64-value {
			return errors.New("invalid budget total")
		}
		*target += value
		return nil
	}
	for id, r := range s.Attempts {
		if r.ID != id || !budgetID(id) || r.ReservedNanoUSD < 0 || r.CostNanoUSD < 0 {
			return out, errors.New("invalid budget attempt")
		}
		var err error
		switch r.State {
		case "reserved", "sending", "unknown":
			err = add(&out.HeldNanoUSD, r.ReservedNanoUSD)
		case "settled":
			err = add(&out.SpentNanoUSD, r.CostNanoUSD)
		case "released":
		default:
			return out, errors.New("invalid budget attempt state")
		}
		if err != nil {
			return out, err
		}
		if r.NeedsReconciliation {
			out.Frozen = true
		}
	}
	if out.SpentNanoUSD > math.MaxInt64-out.HeldNanoUSD {
		return out, errors.New("budget total overflow")
	}
	consumed := out.SpentNanoUSD + out.HeldNanoUSD
	if consumed < s.LimitNanoUSD {
		out.RemainingNanoUSD = s.LimitNanoUSD - consumed
	}
	return out, nil
}
func (a *BudgetAccount) Snapshot(ctx context.Context) (BudgetSnapshot, error) {
	s, err := a.file.Read(ctx)
	if err != nil {
		return BudgetSnapshot{}, err
	}
	return budgetSnapshot(s)
}
func budgetID(id string) bool { b, err := hex.DecodeString(id); return err == nil && len(b) == 16 }
func (a *BudgetAccount) lease(ctx context.Context, id string) (func(), error) {
	if !budgetID(id) {
		return nil, errors.New("invalid attempt ID")
	}
	return durable.Lease(ctx, a.file.Path+".attempts/"+id+".lock")
}

// Recover takes a per-attempt OS lease before changing orphaned state. It does
// not wait for an in-flight call and never holds the account lock over network.
// Reserved orphans are provably unsent; sending orphans are uncertain, not free.
func (a *BudgetAccount) Recover(ctx context.Context) error {
	s, err := a.Snapshot(ctx)
	if err != nil {
		return err
	}
	for id, r := range s.Attempts {
		if r.State != "reserved" && r.State != "sending" {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, time.Millisecond)
		release, e := a.lease(probe, id)
		cancel()
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(e, context.DeadlineExceeded) {
				continue
			}
			return e
		}
		err = a.file.Update(ctx, func(s *BudgetState) error {
			r := s.Attempts[id]
			if r.State == "reserved" {
				r.State = "released"
				r.Reason = "recovered before sending claim"
			}
			if r.State == "sending" {
				r.State = "unknown"
				r.Reason = "process ended after sending claim; billing uncertain"
			}
			r.UpdatedAt = time.Now().UTC()
			s.Attempts[id] = r
			return nil
		})
		release()
		if err != nil {
			return err
		}
	}
	return nil
}

// Reconcile records an explicitly confirmed cost, including a confirmed zero.
// A note is mandatory. The lease prevents overriding an active sender. Repeated
// reconciliation to the same amount is idempotent, conflicting totals fail.
func (a *BudgetAccount) Reconcile(ctx context.Context, id string, costNanoUSD int64, note string) error {
	if costNanoUSD < 0 || strings.TrimSpace(note) == "" || len(note) > 2000 {
		return errors.New("reconciliation requires a nonnegative confirmed cost and note")
	}
	release, err := a.lease(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	return a.file.Update(ctx, func(s *BudgetState) error {
		if _, err := budgetSnapshot(*s); err != nil {
			return err
		}
		r, ok := s.Attempts[id]
		if !ok {
			return errors.New("unknown budget attempt")
		}
		if r.State == "settled" {
			if r.CostNanoUSD == costNanoUSD {
				return nil
			}
			return errors.New("conflicting reconciliation")
		}
		if r.State != "unknown" && r.State != "sending" {
			return errors.New("attempt is not awaiting reconciliation")
		}
		r.State = "settled"
		r.CostNanoUSD = costNanoUSD
		r.Reason = note
		r.NeedsReconciliation = false
		r.UpdatedAt = time.Now().UTC()
		s.Attempts[id] = r
		_, err := budgetSnapshot(*s)
		return err
	})
}

type reservation struct {
	account *BudgetAccount
	id      string
	release func()
}

func (a *BudgetAccount) reserve(ctx context.Context, client Client, p FixedPrice, amount int64) (*reservation, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(id[:])
	release, err := a.lease(ctx, key)
	if err != nil {
		return nil, err
	}
	err = a.file.Update(ctx, func(s *BudgetState) error {
		view, err := budgetSnapshot(*s)
		if err != nil {
			return err
		}
		if view.Frozen {
			return ErrBudgetAccounting
		}
		if amount < 0 || view.SpentNanoUSD+view.HeldNanoUSD > s.LimitNanoUSD || amount > view.RemainingNanoUSD {
			return ErrBudgetExhausted
		}
		now := time.Now().UTC()
		s.Attempts[key] = BudgetAttempt{ID: key, State: "reserved", Provider: client.Name(), Model: client.Model(), ReservedNanoUSD: amount, Price: p, Identity: tracing.Current(ctx), CreatedAt: now, UpdatedAt: now}
		return nil
	})
	if err != nil {
		release()
		return nil, err
	}
	return &reservation{a, key, release}, nil
}
func (r *reservation) update(ctx context.Context, fn func(*BudgetAttempt) error) error {
	return r.account.file.Update(ctx, func(s *BudgetState) error {
		if _, err := budgetSnapshot(*s); err != nil {
			return err
		}
		attempt, ok := s.Attempts[r.id]
		if !ok {
			return errors.New("missing budget reservation")
		}
		if err := fn(&attempt); err != nil {
			return err
		}
		attempt.UpdatedAt = time.Now().UTC()
		s.Attempts[r.id] = attempt
		_, err := budgetSnapshot(*s)
		return err
	})
}
func (r *reservation) sending(ctx context.Context) error {
	return r.update(ctx, func(a *BudgetAttempt) error {
		if a.State != "reserved" {
			return errors.New("attempt already claimed")
		}
		a.State = "sending"
		return nil
	})
}
func (r *reservation) unsent(ctx context.Context) error {
	return r.update(ctx, func(a *BudgetAttempt) error {
		if a.State != "reserved" && a.State != "sending" {
			return errors.New("cannot release sent attempt")
		}
		a.State = "released"
		a.Reason = "sender did not invoke provider"
		return nil
	})
}

// Budgeted belongs immediately around one physical provider attempt, inside
// Resilient, not around the entire retry loop. Each retry/fallback then creates
// a new reservation; primary, curator and workers share Account.
type Budgeted struct {
	Delegate Client
	Account  *BudgetAccount
	prices   map[string]FixedPrice
}

func WrapBudget(delegate Client, account *BudgetAccount, prices map[string]FixedPrice) (*Budgeted, error) {
	if delegate == nil || account == nil {
		return nil, errors.New("budget requires provider and account")
	}
	for c := delegate; c != nil; {
		switch x := c.(type) {
		case *Budgeted:
			return nil, ErrNestedBudget
		case *Resilient:
			return nil, errors.New("budget must wrap each retry client, not Resilient")
		case *Metered:
			c = x.Delegate
		default:
			c = nil
		}
	}
	copy := map[string]FixedPrice{}
	for k, p := range prices {
		if !p.valid() {
			return nil, fmt.Errorf("invalid price for %s", k)
		}
		copy[k] = p
	}
	if _, ok := copy[delegate.Model()]; !ok {
		return nil, fmt.Errorf("missing explicit price for %s", delegate.Model())
	}
	return &Budgeted{delegate, account, copy}, nil
}
func (b *Budgeted) Name() string  { return b.Delegate.Name() }
func (b *Budgeted) Model() string { return b.Delegate.Model() }
func (b *Budgeted) ContextPolicy() (int, int, string) {
	if c, ok := b.Delegate.(ContextProfiler); ok {
		return c.ContextPolicy()
	}
	return (config.Provider{}).ContextPolicy()
}
func (b *Budgeted) NativeTools() bool { return HasNativeTools(b.Delegate) }

type budgetAttemptKey struct{}

// BudgetAttemptID ties a budget reservation to downstream traces or ledgers.
func BudgetAttemptID(ctx context.Context) string {
	id, _ := ctx.Value(budgetAttemptKey{}).(string)
	return id
}

func (b *Budgeted) run(ctx context.Context, maxOutput int, run func(context.Context) (Completion, error)) (Completion, error) {
	if BudgetAttemptID(ctx) != "" {
		return Completion{}, ErrNestedBudget
	}
	if err := ctx.Err(); err != nil {
		return Completion{}, err
	}
	if err := budgetPreflight(b.Delegate); err != nil {
		return Completion{}, err
	}
	window, maxConfigured, _ := b.ContextPolicy()
	if maxOutput <= 0 || maxOutput > maxConfigured || maxOutput >= window {
		return Completion{}, errors.New("invalid model output reservation")
	}
	p := b.prices[b.Model()]
	worst := p
	worst.Input = max(p.Input, p.CacheRead, p.CacheWrite)
	amount, err := worst.cost(Usage{InputTokens: window - maxOutput, OutputTokens: maxOutput, Complete: true})
	if err != nil {
		return Completion{}, err
	}
	reserved, err := b.Account.reserve(ctx, b.Delegate, p, amount)
	if err != nil {
		return Completion{}, err
	}
	defer reserved.release()
	cleanup := func(fn func(context.Context) error) error {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return fn(c)
	}
	if err = reserved.sending(ctx); err != nil {
		releaseErr := cleanup(reserved.unsent)
		return Completion{}, errors.Join(err, releaseErr)
	}
	if err = ctx.Err(); err != nil {
		return Completion{}, errors.Join(err, cleanup(reserved.unsent))
	}
	ctx = context.WithValue(ctx, budgetAttemptKey{}, reserved.id)
	result, callErr := run(ctx)
	actual := result.ActualModel
	if actual == "" {
		actual = b.Model()
	}
	price, knownPrice := b.prices[actual]
	var cost int64
	var accountingErr error
	if result.Usage.Complete && knownPrice {
		cost, accountingErr = price.cost(result.Usage)
	}
	frozen := !knownPrice || accountingErr != nil
	settleErr := cleanup(func(ctx context.Context) error {
		return reserved.update(ctx, func(a *BudgetAttempt) error {
			if a.State != "sending" {
				return errors.New("attempt is not sending")
			}
			a.ActualModel = actual
			a.Usage = result.Usage
			if result.Usage.Complete && knownPrice && accountingErr == nil {
				a.State = "settled"
				a.CostNanoUSD = cost
				a.Price = price
				a.Reason = "settled from complete provider usage"
			} else {
				a.State = "unknown"
				a.Reason = "missing complete provider usage; reservation retained"
				a.NeedsReconciliation = frozen
				if frozen {
					a.Reason = "unpriced model or invalid usage; account frozen for reconciliation"
				}
			}
			return nil
		})
	})
	if settleErr != nil {
		return result, errors.Join(callErr, fmt.Errorf("%w: %v", ErrBudgetState, settleErr))
	}
	if frozen {
		return result, errors.Join(callErr, ErrBudgetAccounting)
	}
	return result, callErr
}
func (b *Budgeted) Complete(ctx context.Context, p string, n int) (Completion, error) {
	return b.run(ctx, n, func(c context.Context) (Completion, error) { return b.Delegate.Complete(c, p, n) })
}
func (b *Budgeted) Stream(ctx context.Context, p string, n int, delta func(string) error) (Completion, error) {
	if delta == nil {
		return Completion{}, errors.New("nil stream callback")
	}
	return b.run(ctx, n, func(c context.Context) (Completion, error) {
		if s, ok := b.Delegate.(StreamClient); ok {
			return s.Stream(c, p, n, delta)
		}
		r, e := b.Delegate.Complete(c, p, n)
		if e == nil {
			e = delta(r.Text)
		}
		return r, e
	})
}
func (b *Budgeted) InspectImage(ctx context.Context, mime string, data []byte, p string, n int) (Completion, error) {
	physical := physicalBudgetClient(b.Delegate)
	if _, ok := physical.(VisionClient); !ok {
		return Completion{}, errors.New("provider does not support vision")
	}
	v, ok := b.Delegate.(VisionClient)
	if !ok {
		return Completion{}, errors.New("provider does not support vision")
	}
	if len(data) == 0 || mime == "" {
		return Completion{}, errors.New("empty image input")
	}
	return b.run(ctx, n, func(c context.Context) (Completion, error) { return v.InspectImage(c, mime, data, p, n) })
}

func physicalBudgetClient(c Client) Client {
	for {
		if m, ok := c.(*Metered); ok {
			c = m.Delegate
			continue
		}
		return c
	}
}
func budgetPreflight(c Client) error {
	var profile *config.Provider
	switch p := physicalBudgetClient(c).(type) {
	case *openAIClient:
		profile = &p.profile
	case *anthropicClient:
		profile = &p.profile
	}
	if profile == nil {
		return nil
	}
	if strings.TrimSpace(profile.APIKey) == "" {
		return errors.New("provider API key is not configured")
	}
	u, err := url.Parse(profile.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid provider base URL")
	}
	return nil
}
func validateBudgetChat(req ChatRequest) error {
	if err := ValidateConversation(req.Messages); err != nil {
		return err
	}
	for _, s := range req.Tools {
		if _, err := tools.JSONSchema(s.Schema); err != nil {
			return err
		}
	}
	_, err := json.Marshal(req)
	return err
}
func (b *Budgeted) Chat(ctx context.Context, req ChatRequest) (Completion, error) {
	c, ok := b.Delegate.(ChatClient)
	if !ok || !c.NativeTools() {
		return Completion{}, errors.New("provider does not support native tools")
	}
	if err := validateBudgetChat(req); err != nil {
		return Completion{}, err
	}
	return b.run(ctx, req.MaxOutputTokens, func(ctx context.Context) (Completion, error) { return c.Chat(ctx, req) })
}
func (b *Budgeted) ChatStream(ctx context.Context, req ChatRequest, delta func(string) error) (Completion, error) {
	if delta == nil {
		return Completion{}, errors.New("nil stream callback")
	}
	c, ok := b.Delegate.(ChatClient)
	if !ok || !c.NativeTools() {
		return Completion{}, errors.New("provider does not support native tools")
	}
	if err := validateBudgetChat(req); err != nil {
		return Completion{}, err
	}
	return b.run(ctx, req.MaxOutputTokens, func(ctx context.Context) (Completion, error) {
		if s, ok := b.Delegate.(ChatStreamClient); ok {
			return s.ChatStream(ctx, req, delta)
		}
		r, e := c.Chat(ctx, req)
		if e == nil {
			e = delta(r.Text)
		}
		return r, e
	})
}
