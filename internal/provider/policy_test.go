package provider

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type policyClient struct {
	name   string
	calls  atomic.Int32
	call   func(context.Context) (Completion, error)
	stream func(context.Context, func(string) error) (Completion, error)
}

func (c *policyClient) Name() string  { return c.name }
func (c *policyClient) Model() string { return c.name + "-model" }
func (c *policyClient) Complete(ctx context.Context, _ string, _ int) (Completion, error) {
	c.calls.Add(1)
	return c.call(ctx)
}
func (c *policyClient) Stream(ctx context.Context, _ string, _ int, emit func(string) error) (Completion, error) {
	c.calls.Add(1)
	return c.stream(ctx, emit)
}

func TestResilientRetriesThenFallsBack(t *testing.T) {
	primary := &policyClient{name: "primary", call: func(ctx context.Context) (Completion, error) {
		if requestedModel(ctx, "") != "primary-model" {
			t.Error("lost requested model identity")
		}
		return Completion{}, &HTTPError{Status: 503}
	}}
	fallback := &policyClient{name: "fallback", call: func(ctx context.Context) (Completion, error) {
		if requestedModel(ctx, "") != "primary-model" {
			t.Error("fallback overwrote requested model")
		}
		return Completion{Text: "ok", ActualModel: "fallback-model"}, nil
	}}
	r := &Resilient{Clients: []Client{primary, fallback}, Retries: 2, Delay: time.Millisecond}
	out, err := r.Complete(context.Background(), "test", 10)
	if err != nil || out.Text != "ok" || primary.calls.Load() != 3 || fallback.calls.Load() != 1 {
		t.Fatalf("out=%+v error=%v calls=%d/%d", out, err, primary.calls.Load(), fallback.calls.Load())
	}
}

func TestResilientDoesNotRetryPermanentFailure(t *testing.T) {
	primary := &policyClient{name: "primary", call: func(context.Context) (Completion, error) { return Completion{}, &HTTPError{Status: 401} }}
	fallback := &policyClient{name: "fallback", call: func(context.Context) (Completion, error) {
		t.Error("permanent error must not route to fallback")
		return Completion{}, nil
	}}
	r := &Resilient{Clients: []Client{primary, fallback}, Retries: 3, Delay: time.Millisecond}
	_, err := r.Complete(context.Background(), "test", 10)
	if err == nil || primary.calls.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatal(err)
	}
}

func TestResilientBackoffHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := &policyClient{name: "primary", call: func(context.Context) (Completion, error) {
		cancel()
		return Completion{}, &HTTPError{Status: 429, RetryAfter: time.Minute}
	}}
	r := &Resilient{Clients: []Client{primary}, Retries: 3, Delay: time.Millisecond}
	_, err := r.Complete(ctx, "test", 10)
	if !errors.Is(err, context.Canceled) || primary.calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d", err, primary.calls.Load())
	}
}

func TestResilientNeverRetriesAfterStreamOutput(t *testing.T) {
	primary := &policyClient{name: "primary", stream: func(_ context.Context, emit func(string) error) (Completion, error) {
		if err := emit("partial"); err != nil {
			return Completion{}, err
		}
		return Completion{Text: "partial"}, io.ErrUnexpectedEOF
	}}
	fallback := &policyClient{name: "fallback", stream: func(context.Context, func(string) error) (Completion, error) {
		t.Error("partial stream rerouted")
		return Completion{}, nil
	}}
	r := &Resilient{Clients: []Client{primary, fallback}, Retries: 2, Delay: time.Millisecond}
	var got string
	_, err := r.Stream(context.Background(), "test", 10, func(s string) error { got += s; return nil })
	if !errors.Is(err, io.ErrUnexpectedEOF) || got != "partial" || primary.calls.Load() != 1 || fallback.calls.Load() != 0 {
		t.Fatalf("error=%v text=%s", err, got)
	}
}

func TestResilientRetriesStreamBeforeAnyOutput(t *testing.T) {
	primary := &policyClient{name: "primary"}
	primary.stream = func(_ context.Context, emit func(string) error) (Completion, error) {
		if primary.calls.Load() == 1 {
			return Completion{}, io.ErrUnexpectedEOF
		}
		return Completion{Text: "complete"}, emit("complete")
	}
	r := &Resilient{Clients: []Client{primary}, Retries: 1, Delay: time.Millisecond}
	var got string
	_, err := r.Stream(context.Background(), "test", 10, func(s string) error { got += s; return nil })
	if err != nil || got != "complete" || primary.calls.Load() != 2 {
		t.Fatalf("error=%v text=%s calls=%d", err, got, primary.calls.Load())
	}
}
