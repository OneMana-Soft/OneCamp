package helpers

import (
	"context"
	"testing"
)

// The mark keeps a change in the workspace: business.EnqueueGitHubSync drops
// a sync when it reads true, so an upgrade's repair of who a task is assigned
// to never changes a GitHub issue's own assignees. It survives cancellation
// (the enqueue runs in a goroutine after its caller returns) and never marks
// the context it came from.
func TestTheWorkspaceOnlyMark(t *testing.T) {
	plain := context.Background()
	if IsWorkspaceOnly(plain) {
		t.Error("an unmarked context reads as workspace-only")
	}
	ctx, cancel := context.WithCancel(WithWorkspaceOnly(plain))
	cancel()
	if !IsWorkspaceOnly(ctx) {
		t.Error("a marked context lost its mark when it was cancelled")
	}
	if IsWorkspaceOnly(plain) {
		t.Error("marking a derived context marked its parent")
	}
}
