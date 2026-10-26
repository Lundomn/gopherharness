package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/scheduler"
	"github.com/Lundomn/gopherharness/internal/store"
)

type serviceRoute struct {
	Session string
	Source  scheduler.Source
}
type serviceRoutes struct{ Routes map[string]serviceRoute }

func (s *Service) resolveSession(ctx context.Context, request scheduler.Request) (string, error) {
	if request.Source.Channel != "" && (request.Source.SenderID == "" || request.Source.ChatID == "") {
		return "", errors.New("channel conversation requires trusted sender and chat identity")
	}
	root := s.opts.Root
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	identity := sha256.Sum256([]byte(request.Conversation))
	release, err := durable.Lease(ctx, filepath.Join(root, ".pico", "service", "route-locks", hex.EncodeToString(identity[:])+".lock"))
	if err != nil {
		return "", err
	}
	defer release()
	file := durable.File[serviceRoutes]{Path: filepath.Join(root, ".pico", "service", "routes.json")}
	state, err := file.Read(ctx)
	if err != nil {
		return "", err
	}
	if route, ok := state.Routes[request.Conversation]; ok {
		if route.Source != request.Source {
			return "", errors.New("conversation is bound to another source identity")
		}
		if route.Session == "" {
			return "", errors.New("conversation route has no session")
		}
		return route.Session, nil
	}
	// Reserve an independent persisted session before publishing its route.
	// Agent construction (including MCP/network I/O) happens after this method
	// returns, never while holding a route state transaction.
	sessions := store.NewSessionStore(root)
	session := sessions.New(root)
	session.MemoryUser = request.Source.SenderID
	if session.MemoryUser == "" {
		session.MemoryUser = "local"
	}
	if err := sessions.Save(session); err != nil {
		return "", err
	}
	err = file.Update(ctx, func(state *serviceRoutes) error {
		if state.Routes == nil {
			state.Routes = map[string]serviceRoute{}
		}
		if existing, ok := state.Routes[request.Conversation]; ok {
			if existing.Source != request.Source {
				return errors.New("conversation source changed during initialization")
			}
			return errors.New("conversation route already initialized")
		}
		state.Routes[request.Conversation] = serviceRoute{Session: session.ID, Source: request.Source}
		return nil
	})
	return session.ID, err
}
