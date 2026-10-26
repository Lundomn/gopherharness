package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/journal"
)

// Reconcile is a local operator API. It deliberately exposes neither a remote
// HTTP admin endpoint nor an option to blindly replay an interrupted tool turn.
func Reconcile(ctx context.Context, j *journal.Journal, kind, id string, decision journal.Resolution, recoveredText string) error {
	if j == nil {
		return errors.New("durable journal required")
	}
	if decision.Action != "recover_result" {
		return j.Resolve(ctx, kind, id, decision, nil)
	}
	if kind != "inbox" || strings.TrimSpace(recoveredText) == "" {
		return errors.New("recover_result requires an inbound record and verified result text")
	}
	entries, err := j.List(ctx, "inbox", "needs_reconcile")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID != id {
			continue
		}
		var event Event
		if err := json.Unmarshal(e.Payload, &event); err != nil {
			return err
		}
		// Route and trace come exclusively from the authenticated durable inbox,
		// never from an operator-provided model response or chat destination.
		message := delivery.Message{ID: id, Source: event.Request.Source, Text: recoveredText, Identity: event.Identity}
		return j.Resolve(ctx, kind, id, decision, message)
	}
	return errors.New("unresolved inbound record not found")
}
