package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A call its caller called off says nothing about the provider. On 11 Oct
// five cancelled embeddings in one second opened the demo's breaker, and the
// next person to ask was refused 503 for half a minute with nothing wrong.
func TestACalledOffCallDoesNotOpenTheBreaker(t *testing.T) {
	cb := NewCircuitBreaker(5, 30*time.Second)
	for i := 0; i < 10; i++ {
		cb.RecordResult(fmt.Errorf(`failed to embed query: ollama: embed request failed: Post "http://ollama:11434/api/embed": %w`, context.Canceled))
	}
	if err := cb.Allow(); err != nil {
		t.Fatalf("the breaker refused a call after cancellations alone: %v (state %s)", err, cb.State())
	}
}

// A provider that doesn't answer in time still counts: that is the failure
// the breaker is for.
func TestAProviderThatTimesOutStillOpensTheBreaker(t *testing.T) {
	cb := NewCircuitBreaker(5, 30*time.Second)
	for i := 0; i < 5; i++ {
		cb.RecordResult(fmt.Errorf("ollama chat: %w", context.DeadlineExceeded))
	}
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("five timeouts left the breaker %s, want it open", cb.State())
	}
}
