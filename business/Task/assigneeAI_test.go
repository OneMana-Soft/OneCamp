package business

import (
	"context"
	"testing"
)

// The AI write of hand-overs is one call for all of them, and none while AI
// is off. AI only, like assigneeAI.go.
func TestTheAIWriteOfHandOversIsOneCallAndNoneWhileAIIsOff(t *testing.T) {
	var calls []map[string]string
	wasOn, wasSet := aiOn, setAIAssignees
	setAIAssignees = func(_ context.Context, to map[string]string) (int, error) {
		calls = append(calls, to)
		return 0, nil
	}
	t.Cleanup(func() { aiOn, setAIAssignees = wasOn, wasSet })
	to := map[string]string{"t1": "u1", "t2": ""}

	aiOn = func() bool { return false }
	if err := aiAssigneeWrite(context.Background(), to); err != nil || len(calls) != 0 {
		t.Fatalf("with AI off the write made %d calls (err %v), want none", len(calls), err)
	}
	aiOn = func() bool { return true }
	if err := aiAssigneeWrite(context.Background(), to); err != nil || len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("with AI on the write made calls %v (err %v), want one of both tasks", calls, err)
	}
}
