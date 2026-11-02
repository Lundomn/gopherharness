package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

func TestDetachedSummaryRevalidatesCurrentReceiptAndRoute(t *testing.T) {
	for _, change := range []string{"summary_sender", "route_sender", "run_id", "valid_text"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			source := scheduler.Source{Channel: "test", ChatID: "group", SenderID: "alice"}
			routes := durable.File[serviceRoutes]{Path: filepath.Join(root, ".pico", "service", "routes.json")}
			if err := routes.Update(ctx, func(s *serviceRoutes) error {
				s.Routes = map[string]serviceRoute{
					"a": {Session: "session-a", Source: source},
					"b": {Session: "session-b", Source: source},
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"a", "b"} {
				if err := saveSummary(ctx, root, summaryReceipt{Status: "pending", Summary: WorkerSummary{Conversation: id, SessionID: "session-" + id, RunID: id, Source: source, Text: "saved summary"}}); err != nil {
					t.Fatal(err)
				}
			}
			var sent []WorkerSummary
			var mutationErr error
			s := &Service{opts: Options{Root: root}, workerCtx: ctx, agents: map[string]*Agent{}}
			s.workerSink = func(_ context.Context, summary WorkerSummary) error {
				sent = append(sent, summary)
				if len(sent) != 1 {
					return nil
				}
				// The startup scan already captured both entries. Change whichever
				// one has not been dispatched yet, without depending on map order.
				other := "a"
				if summary.RunID == "a" {
					other = "b"
				}
				if change == "route_sender" {
					mutationErr = routes.Update(ctx, func(v *serviceRoutes) error {
						route := v.Routes[other]
						route.Source.SenderID = "different-sender"
						v.Routes[other] = route
						return nil
					})
				} else {
					mutationErr = summaryFile(root).Update(ctx, func(v *summaryReceipts) error {
						r := v.Records[other]
						switch change {
						case "summary_sender":
							r.Summary.Source.SenderID = "different-sender"
						case "run_id":
							r.Summary.RunID = "different-run"
						case "valid_text":
							r.Summary.Text = "updated saved summary"
						}
						v.Records[other] = r
						return nil
					})
				}
				return mutationErr
			}
			s.replayDetachedSummaries()
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if change == "valid_text" {
				if len(sent) != 2 || sent[1].Text != "updated saved summary" {
					t.Fatalf("valid current summary was not delivered: %+v", sent)
				}
			} else if len(sent) != 1 {
				t.Fatalf("dispatched a summary whose current identity no longer matches its route: %+v", sent)
			}
		})
	}
}
