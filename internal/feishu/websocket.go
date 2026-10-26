package feishu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Socket owns the SDK run. The startup request's deadline is not its lifetime;
// the gateway closes intake and joins event handlers before releasing its journal.
type Socket struct {
	client *larkws.Client
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	once   sync.Once
	ready  atomic.Bool
}

func (a *Adapter) StartSocket(ctx context.Context, opts ...larkws.ClientOption) (*Socket, error) {
	if a.Config.Transport != "websocket" {
		return nil, errors.New("Feishu websocket transport is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The handler must not be replaceable by an option: it is the trusted intake
	// boundary with the sender/chat allowlists and durable event admission.
	lifetime, cancel := context.WithCancel(ctx)
	s := &Socket{cancel: cancel, done: make(chan struct{})}
	opts = append(opts, larkws.WithEventHandler(dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(a.Receive)),
		larkws.WithHttpClient(a.HTTP),
		larkws.WithOnReady(func() { s.ready.Store(true) }), larkws.WithOnReconnected(func() { s.ready.Store(true) }),
		larkws.WithOnReconnecting(func() { s.ready.Store(false) }), larkws.WithOnDisconnected(func() { s.ready.Store(false) }))
	s.client = larkws.NewClient(a.Config.AppID, a.Config.AppSecret, opts...)
	go func() {
		defer close(s.done)
		defer s.ready.Store(false)
		s.err = s.client.Start(lifetime)
	}()
	return s, nil
}

func (s *Socket) Done() <-chan struct{} { return s.done }
func (s *Socket) Ready() bool           { return s.ready.Load() }

func (s *Socket) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		if errors.Is(s.err, context.Canceled) {
			return nil
		}
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Socket) Close(ctx context.Context) error {
	// Cancelling also covers Close racing with the SDK's beginRun; Close alone
	// could see no run and return just before Start opens a new connection.
	s.once.Do(func() { s.ready.Store(false); s.cancel() })
	s.client.Close()
	return s.Wait(ctx)
}
