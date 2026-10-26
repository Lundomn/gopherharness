package runtime

import "context"

func (a *Agent) ListFiles(ctx context.Context, path string) (string, error) {
	return a.Workspace.ListFiles(ctx, path)
}

func (a *Agent) SearchFiles(ctx context.Context, pattern, path string) (string, error) {
	return a.Workspace.SearchFiles(ctx, pattern, path)
}
