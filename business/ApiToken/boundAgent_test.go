package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// With no database connected the lookup panics inside the model. An identity
// question must answer "no" then, never take the request (or the process) down.
func TestBoundAgentFailsClosed(t *testing.T) {
	agent, why := BoundAgent(context.Background(), uuid.New())
	if agent != nil || why == "" {
		t.Fatalf("BoundAgent without a database = (%v, %q), want a refusal", agent, why)
	}
}
