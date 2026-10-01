package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestShouldContinueSession(t *testing.T) {
	cases := map[string]struct {
		out  *RunOutcome
		want bool
	}{
		"step limit with progress":      {&RunOutcome{StopReason: StopReasonStepLimit, FreshCalls: 2}, true},
		"token limit with progress":     {&RunOutcome{StopReason: StopReasonRunTokenLimit, FreshCalls: 1}, true},
		"step limit, only repeats":      {&RunOutcome{StopReason: StopReasonStepLimit}, false},
		"budget is not a session bound": {&RunOutcome{StopReason: "budget", FreshCalls: 3, Error: "this agent's daily token budget has been reached"}, false},
		"no outcome":                    {nil, false},
	}
	for name, tc := range cases {
		if got := shouldContinueSession(tc.out); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestContinueInNewSession(t *testing.T) {
	old := continueSession
	t.Cleanup(func() { continueSession = old })

	var gotNote string
	var gotMax, calls int
	continueSession = func(_ context.Context, _, _ uuid.UUID, note string, max int, _ *uuid.UUID) (bool, error) {
		calls++
		gotNote, gotMax = note, max
		return true, nil
	}
	job := &model.AgentTask{Id: uuid.New()}
	if !continueInNewSession(context.Background(), job, uuid.New(), &RunOutcome{StopReason: StopReasonStepLimit, FreshCalls: 1}, nil) {
		t.Fatal("a session that made progress continues")
	}
	if gotMax != maxAgentTaskSessions || !strings.Contains(gotNote, "do not repeat") {
		t.Fatalf("note %q max %d", gotNote, gotMax)
	}

	calls = 0
	if continueInNewSession(context.Background(), job, uuid.New(), &RunOutcome{StopReason: StopReasonStepLimit}, nil) || calls != 0 {
		t.Fatal("a session that only repeated itself must not continue")
	}

	continueSession = func(context.Context, uuid.UUID, uuid.UUID, string, int, *uuid.UUID) (bool, error) { return false, nil }
	if continueInNewSession(context.Background(), job, uuid.New(), &RunOutcome{StopReason: StopReasonStepLimit, FreshCalls: 1}, nil) {
		t.Fatal("out of sessions: the caller must finalize")
	}
	continueSession = func(context.Context, uuid.UUID, uuid.UUID, string, int, *uuid.UUID) (bool, error) {
		return false, errors.New("db down")
	}
	if continueInNewSession(context.Background(), job, uuid.New(), &RunOutcome{StopReason: StopReasonStepLimit, FreshCalls: 1}, nil) {
		t.Fatal("a failed transition must fall back to finalizing")
	}
}

func TestCarryOverConversation(t *testing.T) {
	stepLimit := "reached the step limit"
	other := "code_pr no_green"
	convo := `[{"role":"user","content":"Add the rollback steps"},{"role":"assistant","content":"","tool_calls":[{"id":"1"}]},{"role":"tool","tool_call_id":"1","content":"doc found"}]`

	got := carryOverConversation(&model.AgentTask{LastError: &stepLimit, Messages: convo}, "keep going")
	var msgs []map[string]interface{}
	if err := json.Unmarshal([]byte(got), &msgs); err != nil || len(msgs) != 4 {
		t.Fatalf("want the conversation plus the reply, got %s (%v)", got, err)
	}
	if msgs[2]["tool_call_id"] != "1" {
		t.Fatalf("earlier turns must survive intact: %v", msgs[2])
	}
	if msgs[3]["role"] != "user" || !strings.Contains(msgs[3]["content"].(string), "keep going") {
		t.Fatalf("reply turn: %v", msgs[3])
	}

	for name, job := range map[string]*model.AgentTask{
		"finished normally":  {Messages: convo},
		"failed differently": {LastError: &other, Messages: convo},
		"no conversation":    {LastError: &stepLimit, Messages: "[]"},
		"corrupt":            {LastError: &stepLimit, Messages: "{"},
		"nil":                nil,
	} {
		if got := carryOverConversation(job, "more"); got != "" {
			t.Errorf("%s: must start fresh, got %s", name, got)
		}
	}
}

func TestSkipBoundedSummary(t *testing.T) {
	ctx := context.Background()
	if skipBoundedSummary(ctx, 3) {
		t.Fatal("a run outside the durable worker always summarises")
	}
	if !skipBoundedSummary(WithAnotherSession(ctx, true), 3) {
		t.Fatal("a session that will continue needs no closing summary")
	}
	if skipBoundedSummary(WithAnotherSession(ctx, true), 0) {
		t.Fatal("no progress means no next session, so it must summarise")
	}
	if skipBoundedSummary(WithAnotherSession(ctx, false), 3) {
		t.Fatal("the last session must summarise")
	}
}
