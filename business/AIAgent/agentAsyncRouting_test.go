package business

import (
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestShouldRunAsync(t *testing.T) {
	cases := []struct {
		name string
		sig  AsyncSignal
		want bool
	}{
		{"zero value stays sync", AsyncSignal{}, false},
		{"opt-in goes durable", AsyncSignal{OptIn: true}, true},
		{"opt-in wins even with no signals", AsyncSignal{OptIn: true, HandoffEnabled: false}, true},
		{"handoff off: bounded stop stays sync", AsyncSignal{StopReason: StopReasonStepLimit, ToolsSucceeded: 2}, false},
		{"handoff on + step limit + progress hands off", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonStepLimit, ToolsSucceeded: 2}, true},
		{"handoff on + token limit + progress hands off", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonRunTokenLimit, ToolsSucceeded: 1}, true},
		{"handoff on + timeout + progress hands off", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonRunTimeout, ToolsSucceeded: 1}, true},
		{"handoff on but no tool progress stays sync", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonStepLimit, ToolsSucceeded: 0}, false},
		{"handoff on but clean finish stays sync", AsyncSignal{HandoffEnabled: true, StopReason: "", ToolsSucceeded: 2}, false},
		{"handoff on but blocked (needs_human) stays sync", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonBlocked, ToolsSucceeded: 2}, false},
		{"handoff on but budget cap stays sync", AsyncSignal{HandoffEnabled: true, StopReason: StopReasonWorkspaceBudget, ToolsSucceeded: 2}, false},
	}
	for _, c := range cases {
		if got := shouldRunAsync(c.sig); got != c.want {
			t.Errorf("%s: shouldRunAsync(%+v) = %v, want %v", c.name, c.sig, got, c.want)
		}
	}
}

func TestAsyncHandoffEnabled(t *testing.T) {
	for val, want := range map[string]bool{"": false, "false": false, "0": false, "true": true, "1": true, "on": true, "YES": true} {
		t.Setenv("AI_AGENT_ASYNC_HANDOFF", val)
		if got := asyncHandoffEnabled(); got != want {
			t.Errorf("AI_AGENT_ASYNC_HANDOFF=%q: got %v, want %v", val, got, want)
		}
	}
}

// Whether a channel @mention runs durably decides whether a person can stop it,
// correct it mid-run, or watch it at all — those controls exist only for a
// durable job. A tool-less agent stays synchronous (nothing to stop, and the
// queue hop would only add latency).
func TestShouldRunMentionDurably(t *testing.T) {
	cases := []struct {
		name             string
		optIn, hasTools  bool
		deploymentAllows bool
		want             bool
	}{
		{"explicit opt-in wins even with the deployment switch off", true, false, false, true},
		{"tool-capable agent goes durable by default", false, true, true, true},
		{"conversation-only agent stays synchronous", false, false, true, false},
		{"deployment can opt out of durable mentions", false, true, false, false},
		{"nothing set stays synchronous", false, false, false, false},
	}
	for _, c := range cases {
		if got := shouldRunMentionDurably(c.optIn, c.hasTools, c.deploymentAllows); got != c.want {
			t.Errorf("%s: got %t, want %t", c.name, got, c.want)
		}
	}
}

func TestAgentEnabledTools(t *testing.T) {
	cases := map[string]struct {
		stored string
		want   int
	}{
		"two tools":     {`["send_message","create_task"]`, 2},
		"blank entries": {`["send_message","  ",""]`, 1},
		"empty array":   {`[]`, 0},
		"blank blob":    {"   ", 0},
		"malformed":     {`{"nope":true}`, 0}, // never panics, reads as "no tools"
	}
	for name, c := range cases {
		a := &model.AiAgent{EnabledTools: c.stored}
		if got := len(agentEnabledTools(a)); got != c.want {
			t.Errorf("%s: %d tools, want %d", name, got, c.want)
		}
		if got := agentHasTools(a); got != (c.want > 0) {
			t.Errorf("%s: agentHasTools = %t", name, got)
		}
	}
	if agentHasTools(nil) {
		t.Error("a nil agent has no tools")
	}
}

// The slow ack must never overwrite a real message: cancelling an ack that is
// already mid-post has to WAIT for it, so the caller's own output lands after.
func TestSlowAck(t *testing.T) {
	// Cancelled in time: the action never runs.
	fired := make(chan struct{}, 1)
	a := startSlowAck(50*time.Millisecond, func() { fired <- struct{}{} })
	a.Cancel()
	select {
	case <-fired:
		t.Fatal("a cancelled ack must not post")
	case <-time.After(120 * time.Millisecond):
	}
	a.Cancel() // idempotent, and must not block

	// Already firing: Cancel waits for it to finish.
	started := make(chan struct{})
	done := make(chan struct{})
	b := startSlowAck(0, func() {
		close(started)
		time.Sleep(40 * time.Millisecond)
		close(done)
	})
	<-started
	b.Cancel()
	select {
	case <-done:
	default:
		t.Error("Cancel must wait for an ack that is already posting")
	}

	// A nil ack is usable (the run simply never armed one).
	var none *slowAck
	none.Cancel()
	if startSlowAck(time.Second, nil) != nil {
		t.Error("no action means no ack")
	}
}
