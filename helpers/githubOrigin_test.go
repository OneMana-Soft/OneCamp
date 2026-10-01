package helpers

import (
	"context"
	"testing"
)

// The marker is what stops a change received from GitHub being sent straight
// back to it. business.EnqueueGitHubSync drops an enqueue when this reads true,
// so an inverted or missing check turns every inbound webhook into an outbound
// write against a rate limit that is already being hit.
func TestGitHubOriginMarker(t *testing.T) {
	plain := context.Background()
	if IsGitHubOrigin(plain) {
		t.Error("an unmarked context must not read as GitHub-originated")
	}

	marked := WithGitHubOrigin(plain)
	if !IsGitHubOrigin(marked) {
		t.Error("a marked context must read as GitHub-originated")
	}

	// The original must be unaffected: contexts are values, and a webhook
	// marking its own must not mark the caller's.
	if IsGitHubOrigin(plain) {
		t.Error("marking a derived context changed its parent")
	}
}

// Values survive cancellation, which matters because the enqueue runs in a
// goroutine started from a request that may already have completed.
func TestMarkerSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(WithGitHubOrigin(context.Background()))
	cancel()

	if !IsGitHubOrigin(ctx) {
		t.Error("marker lost when the context was cancelled")
	}
}

// A nil context reaches this from goroutines that were handed one by mistake.
// Reporting false is the safe answer: it means "not an echo", so the change is
// treated as a real edit rather than silently dropped.
func TestNilContextIsNotOrigin(t *testing.T) {
	//nolint:staticcheck // deliberately passing nil to pin the behaviour
	if IsGitHubOrigin(nil) {
		t.Error("a nil context must not read as GitHub-originated")
	}
}

// A non-bool value under the key must not be mistaken for a marker.
func TestWrongTypeIsNotOrigin(t *testing.T) {
	ctx := context.WithValue(context.Background(), GitHubOriginContextKey, "yes")
	if IsGitHubOrigin(ctx) {
		t.Error("a non-bool value read as a marker")
	}
}
