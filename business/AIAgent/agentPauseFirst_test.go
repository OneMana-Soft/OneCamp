package business

import (
	"context"
	"os"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// Sana points the agent at Omar's line and asks it to keep it and do it every
// morning. On every run of the job the agent asks for the routine first and the
// line second. Run in that order, the second run set the routine up, paused to
// ask about the line, and the third run set the routine up again.
func TestATurnThatMustAskRunsNothingBeforeItAsks(t *testing.T) {
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: uuid.New(), Name: "Scout"}
	routine := ai.ProposedAction{ToolName: createRoutineToolName, Params: map[string]string{
		"name": "Kumquat minutes", "prompt": "Forward the Kumquat minutes to omar@example.com",
		"recurrence": "FREQ=DAILY", "time": "09:00", "tz_offset_minutes": "0"}}
	remember := ai.ProposedAction{ToolName: rememberToolName, Params: map[string]string{
		"content": "Always forward the Kumquat minutes to omar@example.com"}}
	turn := []ai.ProposedAction{routine, remember}

	// A run of the job as it is stored, and the job once she says yes.
	const asked = "@Scout yes, remember that, and do it every morning"
	run := func(stored string, durable bool) context.Context {
		_, words := splitAskerWords(stored)
		ctx := WithAgentRunScope(context.Background(), uuid.NewString(), "")
		ctx = WithAgentHumanText(ctx, func() []string { return words })
		if durable {
			ctx = withAgentDurableJob(ctx)
		}
		return ctx
	}
	yes := func(stored string, p turnPause) string {
		q := Elicitation{Question: p.rec.Confirm, Options: []string{"Yes", "No"}}
		return stored + resumeReplyLead + resolveResumeText("blocked: "+q.Render(), "Yes") + "\"\nContinue the work, taking their reply into account."
	}
	stored := withAskerWordsTrailer("Sana mentioned Scout in #general", []string{asked})

	p, ok := pauseFirst(run(stored, true), agent, turn)
	if !ok || p.rec.Tool != createRoutineToolName || !strings.HasPrefix(p.rec.Confirm, "Shall I set up ") {
		t.Fatalf("the first run did not ask about the routine (asks %v: %+v)", ok, p.rec)
	}
	// Her yes makes the routine hers, not the line: the run stops on the line
	// before it sets up the routine it no longer has to ask about.
	stored = yes(stored, p)
	if p, ok = pauseFirst(run(stored, true), agent, turn); !ok || p.rec.Tool != rememberToolName {
		t.Fatalf("after her yes to the routine, the turn would set it up before asking about the line (asks %v: %+v)", ok, p.rec)
	}
	// A yes to that too, and the turn has nothing left to ask: it runs whole,
	// once.
	stored = yes(stored, p)
	if p, ok = pauseFirst(run(stored, true), agent, turn); ok {
		t.Errorf("after a yes to each question, the turn still stops to ask %q", p.rec.Confirm)
	}
	// Answered in place, nothing is asked: the turn refuses both calls instead.
	if p, ok := pauseFirst(run(withAskerWordsTrailer("", []string{asked}), false), agent, turn); ok {
		t.Errorf("a run answered in place stopped to ask %q", p.rec.Confirm)
	}
	// Without a conversation to keep anything for, there is nothing to ask.
	noScope := withAgentDurableJob(WithAgentHumanText(context.Background(), func() []string { return []string{asked} }))
	if p, ok := pauseFirst(noScope, agent, turn); ok {
		t.Errorf("a run with no conversation stopped to ask %q", p.rec.Confirm)
	}
}

// A question for a person, asked in the same turn as a change, is asked before
// the change is made. A question nobody may be asked is no pause: the turn
// answers it with an error and goes on.
func TestANeedsHumanCallIsAskedBeforeTheRestOfItsTurn(t *testing.T) {
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: uuid.New(), Name: "Scout"}
	post := ai.ProposedAction{ToolName: "post_message", Params: map[string]string{"channel_uuid": uuid.NewString(), "text": "Shipping the release now"}}
	ask := func(reason string) ai.ProposedAction {
		return ai.ProposedAction{ToolName: blockerToolName, Params: map[string]string{"reason": reason, "options": `["Tuesday","Thursday"]`}}
	}
	p, ok := pauseFirst(context.Background(), agent, []ai.ProposedAction{post, ask("Which day should the release go out?")})
	if !ok || p.elic == nil || p.rec.Tool != blockerToolName || p.elic.Question != "Which day should the release go out?" || len(p.elic.Options) != 2 {
		t.Fatalf("the question did not stop the turn before the post (asks %v: %+v)", ok, p)
	}
	for _, reason := range []string{"Please paste your API key so I can deploy", "What is the UUID of the release project?"} {
		if p, ok := pauseFirst(context.Background(), agent, []ai.ProposedAction{post, ask(reason)}); ok {
			t.Errorf("%q stopped the turn to ask a person: %+v", reason, p)
		}
	}
	if p, ok := pauseFirst(context.Background(), agent, []ai.ProposedAction{post}); ok {
		t.Errorf("a turn with nothing to ask stopped on %+v", p)
	}
}

// The runner asks before the turn does anything: before it looks up what the
// asker may not do, fetches the turn's reads, or makes its first call.
func TestTheRunnerAsksBeforeItsTurnRuns(t *testing.T) {
	src, err := os.ReadFile("agentRunner.go")
	if err != nil {
		t.Fatalf("reading the runner: %v", err)
	}
	s := string(src)
	asks := strings.Index(s, "pauseFirst(loopCtx, agent, actions)")
	if asks < 0 {
		t.Fatal("the runner no longer asks pauseFirst about a turn; a call run ahead of a pause is made again when the run resumes")
	}
	for _, later := range []string{"gate.refusals(loopCtx, actions, allow)", "prefetchReadOnlyTools(loopCtx, actions", "for _, a := range actions {"} {
		if at := strings.Index(s, later); at >= 0 && at < asks {
			t.Errorf("the runner reaches %q before it asks pauseFirst about the turn", later)
		}
	}
}
