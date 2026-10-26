package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
)

// Delivery-only startup recovery does not construct an Agent, acquire a model
// client, enqueue another task, or rerun a tool. A durable route must attest the
// conversation, session and sender before its stored text can leave the service.
func (s *Service) replayDetachedSummaries() {
	s.mu.Lock()
	sink := s.workerSink
	closed := s.workerClosed
	root := s.opts.Root
	loaded := map[string]bool{}
	for cid := range s.agents {
		loaded[cid] = true
	}
	s.mu.Unlock()
	if sink == nil || closed || s.workerCtx.Err() != nil {
		return
	}
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return
		}
	}
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(root, ".pico", "workers", "summaries.json"))
	if err != nil {
		return
	}
	var state summaryReceipts
	if json.Unmarshal(data, &state) != nil {
		return
	}
	data, err = os.ReadFile(filepath.Join(root, ".pico", "service", "routes.json"))
	if err != nil {
		return
	}
	var routes serviceRoutes
	if json.Unmarshal(data, &routes) != nil {
		return
	}
	for id, r := range state.Records {
		if s.workerCtx.Err() != nil {
			return
		}
		if loaded[r.Summary.Conversation] || (r.Status != "pending" && r.Status != "generating") {
			continue
		}
		route, ok := routes.Routes[r.Summary.Conversation]
		if !ok || route.Session != r.Summary.SessionID || route.Source != r.Summary.Source || id != r.Summary.RunID || id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
			continue
		}
		if route.Source.Channel != "" && (route.Source.ChatID == "" || route.Source.SenderID == "") {
			continue
		}
		ctx, cancel := context.WithTimeout(s.workerCtx, 10*time.Second)
		hash := sha256.Sum256([]byte(id))
		release, e := durable.Lease(ctx, filepath.Join(root, ".pico", "workers", "delivery-locks", hex.EncodeToString(hash[:])+".lock"))
		if e != nil {
			cancel()
			continue
		}
		current, e := summaryFile(root).Read(ctx)
		if e == nil {
			r = current.Records[id]
		}
		if e == nil && (r.Status == "pending" || r.Status == "generating") {
			r, e = recoverSummary(ctx, root, r)
			if e == nil {
				if e = sink(ctx, r.Summary); e == nil {
					_ = settleSummary(ctx, root, id, "accepted", "")
				}
			}
		}
		release()
		cancel()
	}
}
