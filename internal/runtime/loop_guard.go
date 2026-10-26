package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Lundomn/gopherharness/internal/model"
)

// Only repeated failures count; successful repeated reads are valid checks.
type loopGuard struct {
	Failures   map[string]int
	FinalOnly  bool
	FinalCalls int
	Reason     string
}

func (g *loopGuard) observe(call model.ToolCall, result model.ToolResult) {
	if result.Status == "ok" || result.Status == "pending" {
		return
	}
	if g.Failures == nil {
		g.Failures = map[string]int{}
	}
	data, _ := json.Marshal([]any{call.Name, call.Args, result.ErrorCode, result.Error})
	h := sha256.Sum256(data)
	key := hex.EncodeToString(h[:])
	g.Failures[key]++
	if g.Failures[key] >= 2 {
		g.FinalOnly = true
		g.Reason = "same tool and arguments failed with the same error twice"
	}
}
