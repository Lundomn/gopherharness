package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"time"
)

type HTTPError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("provider HTTP %d: %s", e.Status, e.Detail) }
func Retryable(err error) bool {
	for _, fatal := range []error{ErrBudgetExhausted, ErrBudgetAccounting, ErrBudgetState, ErrNestedBudget} {
		if errors.Is(err, fatal) {
			return false
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Status == 408 || h.Status == 429 || h.Status >= 500
	}
	var n net.Error
	return errors.As(err, &n) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}
func ContextOverflow(err error) bool {
	var h *HTTPError
	if !errors.As(err, &h) {
		return false
	}
	if h.Status == 413 {
		return true
	}
	if h.Status != 400 {
		return false
	}
	detail := strings.ToLower(h.Detail)
	for _, code := range []string{"context_length_exceeded", "maximum context length", "prompt is too long", "exceeds the context window", "input length and max_tokens exceed"} {
		if strings.Contains(detail, code) {
			return true
		}
	}
	return false
}

type requestKey struct{}

func requested(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, requestKey{}, model)
}
func requestedModel(ctx context.Context, fallback string) string {
	if s, ok := ctx.Value(requestKey{}).(string); ok {
		return s
	}
	return fallback
}

type Resilient struct {
	Clients []Client
	Retries int
	Delay   time.Duration
}

func (r *Resilient) Name() string  { return r.Clients[0].Name() }
func (r *Resilient) Model() string { return r.Clients[0].Model() }
func (r *Resilient) call(ctx context.Context, run func(Client) (Completion, error)) (Completion, error) {
	if len(r.Clients) == 0 {
		return Completion{}, fmt.Errorf("no provider clients configured")
	}
	var out Completion
	var err error
	for _, c := range r.Clients {
		for n := 0; n <= r.Retries; n++ {
			if e := ctx.Err(); e != nil {
				return out, e
			}
			out, err = run(c)
			if err == nil {
				return out, nil
			}
			if !Retryable(err) {
				return out, err
			}
			if n == r.Retries {
				break
			}
			wait := min(30*time.Second, max(time.Millisecond, r.Delay)*time.Duration(1<<min(n, 8)))
			wait += time.Duration(rand.Int64N(max(1, int64(wait/4))))
			var h *HTTPError
			if errors.As(err, &h) {
				wait = max(wait, min(time.Minute, h.RetryAfter))
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return out, ctx.Err()
			}
		}
	}
	return out, err
}
func (r *Resilient) Complete(ctx context.Context, p string, n int) (Completion, error) {
	ctx = requested(ctx, r.Model())
	return r.call(ctx, func(c Client) (Completion, error) { return c.Complete(ctx, p, n) })
}

type partialStream struct{ err error }

func (e partialStream) Error() string { return e.err.Error() }
func (r *Resilient) Stream(ctx context.Context, p string, n int, delta func(string) error) (Completion, error) {
	ctx = requested(ctx, r.Model())
	out, err := r.call(ctx, func(c Client) (Completion, error) {
		emitted := false
		callbackFailed := false
		emit := func(s string) error {
			if s != "" {
				emitted = true
			}
			e := delta(s)
			callbackFailed = e != nil
			return e
		}
		var v Completion
		var e error
		if s, ok := c.(StreamClient); ok {
			v, e = s.Stream(ctx, p, n, emit)
		} else {
			v, e = c.Complete(ctx, p, n)
			if e == nil {
				e = emit(v.Text)
			}
		}
		if e != nil && (emitted || callbackFailed) {
			return v, partialStream{e}
		}
		return v, e
	})
	var partial partialStream
	if errors.As(err, &partial) {
		err = partial.err
	}
	return out, err
}
func (r *Resilient) InspectImage(ctx context.Context, mime string, data []byte, p string, n int) (Completion, error) {
	ctx = requested(ctx, r.Model())
	return r.call(ctx, func(c Client) (Completion, error) {
		if v, ok := c.(VisionClient); ok {
			return v.InspectImage(ctx, mime, data, p, n)
		}
		return Completion{}, fmt.Errorf("vision unsupported")
	})
}
