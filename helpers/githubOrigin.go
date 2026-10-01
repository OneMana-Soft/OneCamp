package helpers

import "context"

// WithGitHubOrigin marks a context as applying a change that arrived from
// GitHub. Call it once, where the webhook is handled; everything downstream
// inherits it.
func WithGitHubOrigin(ctx context.Context) context.Context {
	return context.WithValue(ctx, GitHubOriginContextKey, true)
}

// IsGitHubOrigin reports whether this work is applying a change from GitHub.
//
// Used by the outbound sync to drop the echo. Reads as false for any context
// that never passed through the webhook, which is every ordinary user edit.
func IsGitHubOrigin(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	origin, _ := ctx.Value(GitHubOriginContextKey).(bool)
	return origin
}

// WithSystemRead marks a read as performed by the server itself, with no user
// whose access could be checked. Use it only for background work.
func WithSystemRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, SystemReadContextKey, true)
}

// IsSystemRead reports whether this read is the server's own.
func IsSystemRead(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	system, _ := ctx.Value(SystemReadContextKey).(bool)
	return system
}
