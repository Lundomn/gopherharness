package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Lundomn/gopherharness/internal/durable"
	"github.com/Lundomn/gopherharness/internal/model"
	"github.com/Lundomn/gopherharness/internal/worker"
)

type workerSummaryRunKey struct{}
type summaryReceipt struct {
	Summary   WorkerSummary
	Status    string
	CreatedAt time.Time
	Results   []worker.Receipt
	LastError string
}
type summaryReceipts struct{ Records map[string]summaryReceipt }

func summaryFile(root string) durable.File[summaryReceipts] {
	return durable.File[summaryReceipts]{Path: filepath.Join(root, ".pico", "workers", "summaries.json")}
}
func saveSummary(ctx context.Context, root string, r summaryReceipt) error {
	return summaryFile(root).Update(ctx, func(s *summaryReceipts) error {
		if s.Records == nil {
			s.Records = map[string]summaryReceipt{}
		}
		if old, ok := s.Records[r.Summary.RunID]; ok && (old.Status == "revoked" || old.Status == "accepted") {
			return nil
		}
		s.Records[r.Summary.RunID] = r
		return nil
	})
}
func nextSummary(ctx context.Context, root, cid string) (summaryReceipt, bool, error) {
	s, err := summaryFile(root).Read(ctx)
	if err != nil {
		return summaryReceipt{}, false, err
	}
	var pending []summaryReceipt
	for _, r := range s.Records {
		if r.Summary.Conversation == cid && (r.Status == "generating" || r.Status == "pending") {
			pending = append(pending, r)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })
	if len(pending) == 0 {
		return summaryReceipt{}, false, nil
	}
	return pending[0], true, nil
}
func settleSummary(ctx context.Context, root, id, status, reason string) error {
	return summaryFile(root).Update(ctx, func(s *summaryReceipts) error {
		r, ok := s.Records[id]
		if !ok {
			return errors.New("summary receipt missing")
		}
		if r.Status == "revoked" || r.Status == "accepted" {
			return nil
		}
		r.Status, r.LastError = status, reason
		s.Records[id] = r
		return nil
	})
}
func revokeSummaries(ctx context.Context, root, cid string) error {
	return summaryFile(root).Update(ctx, func(s *summaryReceipts) error {
		for id, r := range s.Records {
			if r.Summary.Conversation == cid && r.Status != "accepted" {
				r.Status = "revoked"
				s.Records[id] = r
			}
		}
		return nil
	})
}

// A generation intent is written before a model starts. After restart we reuse
// its successful report, or return the recorded raw result with an interruption
// label. The model and tools are not rerun to repair a delivery handoff.
func recoverSummary(ctx context.Context, root string, r summaryReceipt) (summaryReceipt, error) {
	if r.Status != "generating" {
		return r, nil
	}
	data, err := os.ReadFile(filepath.Join(root, ".pico", "runs", r.Summary.RunID, "report.json"))
	var report model.RunReport
	if err == nil && json.Unmarshal(data, &report) == nil && report.RunID == r.Summary.RunID && report.SessionID == r.Summary.SessionID && report.Status == model.StatusCompleted {
		r.Summary.Text = report.FinalAnswer
	} else {
		var b strings.Builder
		b.WriteString("后台结果已保存；自动汇总未完成，以下为原始结果，不会重新执行子任务。\n")
		for _, result := range r.Results {
			b.WriteString(result.ID + " [" + result.Status + "]: " + result.Result + result.Error + "\n")
		}
		runes := []rune(b.String())
		if len(runes) > 16000 {
			runes = runes[:16000]
		}
		r.Summary.Text = string(runes)
	}
	r.Status = "pending"
	return r, saveSummary(ctx, root, r)
}
