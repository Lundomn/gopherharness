package gatewayapp

import (
	"context"
	"errors"
	"strings"

	"github.com/Lundomn/gopherharness/internal/delivery"
	"github.com/Lundomn/gopherharness/internal/runtime"
)

// Background summaries retain the authenticated source held by Service. They
// cannot invent a destination from model text. The summary's fixed run ID is
// the idempotency key for this independent asynchronous reply.
func (s *Server) postWorkerSummary(ctx context.Context, summary runtime.WorkerSummary) error {
	if summary.RunID == "" || strings.TrimSpace(summary.Text) == "" || summary.Source.Channel != "feishu" || !s.adapter.Allowed(summary.Source) {
		return errors.New("worker summary requires a stable run ID, text and allowed channel owner")
	}
	return s.gateway.Hub.PostDurable(ctx, delivery.Message{
		ID: "worker:" + summary.RunID, Source: summary.Source, Text: summary.Text, Identity: summary.Identity,
	})
}
