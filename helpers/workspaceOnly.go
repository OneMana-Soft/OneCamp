package helpers

import "context"

// workspaceOnlyKey marks a change that stays in the workspace.
type workspaceOnlyKey struct{}

// WithWorkspaceOnly marks ctx as making a change that stays in the workspace:
// no tool a task is linked to hears of it. An upgrade's repair of who a task
// is assigned to corrects OneCamp's own records, while a GitHub issue's
// assignees are the issue's own, shared with everyone who sees it there.
func WithWorkspaceOnly(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, workspaceOnlyKey{}, true)
}

// IsWorkspaceOnly reports whether ctx was marked WithWorkspaceOnly. The
// outbound GitHub sync reads it.
func IsWorkspaceOnly(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(workspaceOnlyKey{}).(bool)
	return v
}
