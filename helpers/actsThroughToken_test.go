package helpers

import (
	"context"
	"testing"
)

// A call is a token's when its token's id is on the context (the public API,
// MCP), or when it was marked as one (an agent run a token queued, resumed);
// a session's never is.
func TestActsThroughTokenIsTheTokensIdOrItsMark(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"a session", context.Background(), false},
		{"the public API or MCP", context.WithValue(context.Background(), ApiTokenIDContextKey, "token-1"), true},
		{"an empty token id", context.WithValue(context.Background(), ApiTokenIDContextKey, ""), false},
		{"a run a token queued, resumed", WithActsThroughToken(context.Background()), true},
	} {
		if got := ActsThroughToken(c.ctx); got != c.want {
			t.Errorf("%s: ActsThroughToken = %v, want %v", c.name, got, c.want)
		}
	}
}
