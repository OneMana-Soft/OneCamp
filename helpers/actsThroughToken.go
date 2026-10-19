package helpers

import "context"

// An API token acts at most as a member. A token carries none of its owner's
// admin powers, neither the workspace's (what an admin may do over everyone's
// tables, data sources and docs) nor a role on one place (a team's admins
// creating in it, a channel's admins posting where only they may): what only
// an admin may do, they do in the app, as themselves (the owner's decision of
// 16 Oct 2026). ActsThroughToken is the one test for it, asked everywhere a
// token's call builds an actor or a subject, or decides a write by a role:
// the public API, MCP, the AI's executors MCP runs, and an agent run a token
// asked for. helpers/actsThroughTokenGuard_test.go holds that code to it.

// actsThroughTokenKey marks a context whose work an API token asked for, when
// the token's own id is no longer on it: an agent run a token queued, resumed
// from its job on a worker (business/AIAgent askedForJob).
const actsThroughTokenKey ContextKey = "actsThroughToken"

// ActsThroughToken is whether ctx is a call an API token made, or work one
// asked for, and so acts at most as a member: the token's id the API token
// middleware and the MCP authorizer set (GetApiTokenID), or the mark
// WithActsThroughToken leaves. A session in the app is neither.
func ActsThroughToken(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if GetApiTokenID(ctx) != "" {
		return true
	}
	marked, _ := ctx.Value(actsThroughTokenKey).(bool)
	return marked
}

// WithActsThroughToken marks ctx as work an API token asked for, so
// ActsThroughToken holds on it without the token's id: for an agent run
// resumed from a job a token's request queued.
func WithActsThroughToken(ctx context.Context) context.Context {
	return context.WithValue(ctx, actsThroughTokenKey, true)
}
