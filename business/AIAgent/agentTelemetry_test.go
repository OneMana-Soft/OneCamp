package business

// What the exported span must say, pinned.
//
// This is the one record that leaves the product. Everything else here is read
// by a person inside OneCamp who can go and look at the run; a span lands in
// somebody else's alerting, where nobody can. Two properties matter more than
// the rest and neither is obvious enough to survive a refactor unpinned:
//
//  1. A governance refusal is NOT an error. It is the product working. Marking
//     it Error would page an on-call engineer every time policy did its job,
//     and the fastest way for a team to stop trusting a governance signal is
//     for it to wake them up when nothing is wrong.
//  2. The timestamps are the run's own. A span stamped now() collapses every
//     run to a point and puts it at the wrong end of the trace, which quietly
//     makes the export useless for the thing it exists for.

import (
	"context"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans installs an in-memory tracer provider for one test and restores
// the previous global afterwards, so ordering between tests cannot matter.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func attrsOf(t *testing.T, s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	t.Helper()
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func testAgent() *model.AiAgent {
	return &model.AiAgent{Id: uuid.New(), Name: "Release Triage"}
}

func TestEmitRunSpanGovernanceBlockIsNotAnError(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-4 * time.Minute)

	emitRunSpan(context.Background(), finishedRun{
		Agent:            testAgent(),
		RunID:            uuid.New(),
		TriggerSource:    "schedule",
		Status:           model.RunSucceeded,
		GovernanceBlocks: []string{"denied_by_policy on delete_task"},
		StartedAt:        start,
		EndedAt:          start.Add(4 * time.Minute),
	})

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected exactly one span, got %d", len(spans))
	}
	got := spans[0]
	if got.Name() != "invoke_agent" {
		t.Errorf("span name = %q, want invoke_agent", got.Name())
	}
	if got.Status().Code != codes.Ok {
		t.Errorf("a governance-blocked run reported %v; a refusal is the system working, not an incident", got.Status().Code)
	}

	a := attrsOf(t, got)
	if v := a["onecamp.agent.governance_blocked"].AsStringSlice(); len(v) != 1 || v[0] != "denied_by_policy on delete_task" {
		t.Errorf("governance_blocked = %v, want the refusal", v)
	}
	if n := a["onecamp.agent.governance_blocks_count"].AsInt64(); n != 1 {
		t.Errorf("governance_blocks_count = %d, want 1", n)
	}
	if a["gen_ai.operation.name"].AsString() != "invoke_agent" {
		t.Error("gen_ai.operation.name missing: without it this does not land in a GenAI dashboard")
	}
	if a["gen_ai.agent.name"].AsString() != "Release Triage" {
		t.Errorf("gen_ai.agent.name = %q", a["gen_ai.agent.name"].AsString())
	}
	if a["onecamp.agent.trigger"].AsString() != "schedule" {
		t.Error("trigger missing: whether a person asked is the first thing a reviewer wants")
	}
}

func TestEmitRunSpanUsesTheRunsOwnTimestamps(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-90 * time.Second)
	end := start.Add(75 * time.Second)

	emitRunSpan(context.Background(), finishedRun{
		Agent: testAgent(), RunID: uuid.New(),
		Status: model.RunSucceeded, StartedAt: start, EndedAt: end,
	})

	got := rec.Ended()[0]
	if !got.StartTime().Equal(start) || !got.EndTime().Equal(end) {
		t.Fatalf("span ran %s..%s, want %s..%s", got.StartTime(), got.EndTime(), start, end)
	}
	if d := got.EndTime().Sub(got.StartTime()); d != 75*time.Second {
		t.Errorf("span duration = %s, want 75s; a span stamped now() says the wrong thing about when the work happened", d)
	}
}

func TestEmitRunSpanFailedRunCarriesTheError(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-time.Minute)

	emitRunSpan(context.Background(), finishedRun{
		Agent: testAgent(), RunID: uuid.New(),
		Status: model.RunFailed, Error: "the model refused to stop",
		FailedTools: []string{"create_task"},
		StartedAt:   start, EndedAt: time.Now(),
	})

	got := rec.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Fatalf("failed run reported %v, want Error", got.Status().Code)
	}
	if got.Status().Description != "the model refused to stop" {
		t.Errorf("description = %q, want the run's error", got.Status().Description)
	}
	if v := attrsOf(t, got)["onecamp.agent.tools_failed"].AsStringSlice(); len(v) != 1 {
		t.Errorf("tools_failed = %v", v)
	}
}

// A stopped run is a person pressing stop. Reporting it as an error turns a
// deliberate human action into somebody else's 3am page.
func TestEmitRunSpanStoppedRunIsNotAnError(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-time.Minute)

	emitRunSpan(context.Background(), finishedRun{
		Agent: testAgent(), RunID: uuid.New(),
		Status: model.RunStopped, StartedAt: start, EndedAt: time.Now(),
	})

	if got := rec.Ended()[0].Status().Code; got != codes.Ok {
		t.Errorf("stopped run reported %v, want Ok", got)
	}
}

// Absence has to look like absence. An empty list emitted as an attribute makes
// every clean run carry noise, and a zero uuid makes every run without a row
// look like the same run.
func TestEmitRunSpanOmitsWhatItDoesNotHave(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-time.Second)

	emitRunSpan(context.Background(), finishedRun{
		Agent: testAgent(), RunID: uuid.Nil,
		Status: model.RunSucceeded, StartedAt: start, EndedAt: time.Now(),
	})

	a := attrsOf(t, rec.Ended()[0])
	for _, k := range []attribute.Key{
		"onecamp.agent.run_id",
		"onecamp.agent.trigger",
		"onecamp.agent.tools_succeeded",
		"onecamp.agent.tools_failed",
		"onecamp.agent.governance_blocked",
	} {
		if _, ok := a[k]; ok {
			t.Errorf("%s was emitted with nothing to say", k)
		}
	}
	// The counts still appear, because "zero tools failed" is a fact worth
	// aggregating and a missing count is not the same as a count of zero.
	if n, ok := a["onecamp.agent.tools_failed_count"]; !ok || n.AsInt64() != 0 {
		t.Error("tools_failed_count should be present and zero")
	}
}

// A run with no start time is not a run this can describe. Guessing one would
// put a fabricated duration into somebody's trace.
func TestEmitRunSpanSkipsWhatItCannotDescribe(t *testing.T) {
	rec := recordSpans(t)
	emitRunSpan(context.Background(), finishedRun{Agent: testAgent(), Status: model.RunSucceeded})
	emitRunSpan(context.Background(), finishedRun{StartedAt: time.Now(), Status: model.RunSucceeded})
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("emitted %d spans for runs it cannot describe, want 0", n)
	}
}

// An end time before the start would render as a negative duration, which most
// backends drop silently. Falling forward to now keeps the span.
func TestEmitRunSpanRepairsBackwardsTimestamps(t *testing.T) {
	rec := recordSpans(t)
	start := time.Now().Add(-time.Minute)

	emitRunSpan(context.Background(), finishedRun{
		Agent: testAgent(), RunID: uuid.New(), Status: model.RunSucceeded,
		StartedAt: start, EndedAt: start.Add(-time.Hour),
	})

	got := rec.Ended()[0]
	if !got.EndTime().After(got.StartTime()) {
		t.Fatalf("span ends before it starts: %s..%s", got.StartTime(), got.EndTime())
	}
}
