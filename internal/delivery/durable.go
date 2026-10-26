package delivery

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Lundomn/gopherharness/internal/tracing"
)

// PostDurable records an already-computed result before scheduling delivery.
// A failed queue admission leaves the same pending payload for retry/restart;
// delivery must never ask the runtime to regenerate the result. A stable ID is
// permanently bound to its original destination and text, even after delivery.
func (h *Hub) PostDurable(ctx context.Context, msg Message) error {
	if h.Journal == nil || msg.ID == "" || msg.Source.Channel == "" || msg.Source.ChatID == "" {
		return errors.New("durable delivery requires a journal, stable ID and reply target")
	}
	if msg.Identity.TraceID == "" {
		msg.Identity = tracing.Current(ctx)
	}
	created, err := h.Journal.Put(ctx, "outbox", msg.ID, msg)
	if err != nil {
		return err
	}
	if !created {
		e, ok, err := h.Journal.Get(ctx, "outbox", msg.ID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("durable delivery record disappeared")
		}
		var saved Message
		if err := json.Unmarshal(e.Payload, &saved); err != nil {
			return err
		}
		if saved.ID != msg.ID || saved.Source != msg.Source || saved.Text != msg.Text {
			return errors.New("delivery ID already belongs to a different result or destination")
		}
		// Sending/unknown outcomes are not automatically replayed. The delivery
		// ledger already owns them and explicit reconciliation handles unknowns.
		if e.State != "pending" {
			return nil
		}
		msg = saved
	}
	return h.Post(ctx, msg)
}
