package runtime

import (
	"context"
	"sync"
)

// contextMutex serializes an Agent without stranding cancelled queued callers.
// Close uses Lock to wait for the actual owner, never abandoning cleanup.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) init() {
	m.once.Do(func() { m.token = make(chan struct{}, 1); m.token <- struct{}{} })
}
func (m *contextMutex) Lock()   { m.init(); <-m.token }
func (m *contextMutex) Unlock() { m.token <- struct{}{} }
func (m *contextMutex) LockContext(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}
