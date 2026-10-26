package runtime

import (
	"context"
	"os"

	"github.com/Lundomn/gopherharness/internal/memory"
)

func (a *Agent) memoryInput(ctx context.Context, query string) (memory.InputSegment, error) {
	builder := memory.InputBuilder{Backend: a.Facts, Scope: a.FactScope, Enabled: a.Facts != nil, TopK: 5}
	builder.ReadHost = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		data, err := a.Workspace.ReadFile(ctx, memory.ProfileRelativePath(a.FactScope.User))
		if os.IsNotExist(err) {
			return "", nil
		}
		return string(data), err
	}
	return builder.Build(ctx, query)
}
