package helpers

import "context"

// agentWriteKey marks work an AI agent does in its own name: its replies and
// status notes, and whatever a run's tools change.
type agentWriteKey struct{}

// WithAgentWrite marks ctx as an agent's write. Set where a run starts and
// where the agent posts outside one; everything downstream inherits it.
func WithAgentWrite(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, agentWriteKey{}, true)
}

// IsAgentWrite reports whether this write is an agent's. The outbound GitHub
// sync reads it, so an agent's comment on a linked task stays in the workspace.
func IsAgentWrite(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(agentWriteKey{}).(bool)
	return v
}
