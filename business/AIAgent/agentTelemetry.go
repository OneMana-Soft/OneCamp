package business

// Agent runs as OpenTelemetry spans.
//
// The governance record already existed and only existed HERE. A team whose
// agents are misbehaving looks at the place their other incidents are, and this
// product answered "what did the agent do, and what was it refused" on a screen
// nobody has open. The industry named the problem this year: agent sprawl
// producing incidents nobody can attribute, and governance that lives in
// documents rather than in something a runtime emits.
//
// A span per RUN, emitted where the audit row is written, which is the single
// place a run ends and already carries everything needed. Deliberately not
// instrumentation threaded through the tool loop: that is invasive, it is the
// hot path, and per-call detail already lives in the transcript this span
// points at.
//
// ATTRIBUTE NAMING. gen_ai.* names come from the OpenTelemetry GenAI semantic
// conventions so this lands in an existing dashboard rather than as an unknown
// shape. Their agent and tool conventions are still experimental, and there is
// no standard at all for "governance refused this", so anything without a
// standard name is namespaced onecamp.* rather than guessing at a key that may
// mean something else later.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation in the receiving backend.
const tracerName = "github.com/akashc777/OneCamp/business/AIAgent"

// emitRunSpan records a finished agent run as a span.
//
// Timestamps are the run's own, not now(): a run that took four minutes must
// appear as four minutes beside the other spans in that window, or the trace
// says something false about when the work happened.
//
// Best effort and non-blocking by construction. A tracer with no exporter
// configured is a no-op recorder, which is the normal case for a self-hoster
// who has never set OTEL_EXPORTER_OTLP_ENDPOINT, and costs nothing.
func emitRunSpan(ctx context.Context, r finishedRun) {
	if r.Agent == nil || r.StartedAt.IsZero() {
		return
	}
	endedAt := r.EndedAt
	if endedAt.IsZero() || endedAt.Before(r.StartedAt) {
		endedAt = time.Now()
	}

	attrs := []attribute.KeyValue{
		// Standard, so an existing GenAI dashboard groups these without help.
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("gen_ai.agent.id", r.Agent.Id.String()),
		attribute.String("gen_ai.agent.name", r.Agent.Name),

		// No standard exists for these, so they are namespaced rather than
		// squatting on a key the specification may define differently.
		attribute.String("onecamp.agent.run_status", r.Status),
		attribute.Bool("onecamp.agent.dry_run", r.DryRun),
		attribute.Int("onecamp.agent.tools_succeeded_count", len(r.ToolsSucceeded)),
		attribute.Int("onecamp.agent.tools_failed_count", len(r.FailedTools)),
		attribute.Int("onecamp.agent.governance_blocks_count", len(r.GovernanceBlocks)),
	}
	// uuid.Nil is not a run id, it is the absence of one: CreateRun can fail and
	// the run still happens. Emitting zeroes would make every such run look like
	// the same run.
	if r.RunID != uuid.Nil {
		attrs = append(attrs, attribute.String("onecamp.agent.run_id", r.RunID.String()))
	}
	if t := r.TriggerSource; t != "" {
		attrs = append(attrs, attribute.String("onecamp.agent.trigger", t))
	}
	// Lists only when non-empty: an attribute present on every span with an
	// empty value is noise that costs storage and tells nobody anything.
	if len(r.ToolsSucceeded) > 0 {
		attrs = append(attrs, attribute.StringSlice("onecamp.agent.tools_succeeded", r.ToolsSucceeded))
	}
	if len(r.FailedTools) > 0 {
		attrs = append(attrs, attribute.StringSlice("onecamp.agent.tools_failed", r.FailedTools))
	}
	if len(r.GovernanceBlocks) > 0 {
		// The whole point of exporting this: a refusal is the most interesting
		// thing an agent does, and it was previously invisible outside OneCamp.
		attrs = append(attrs, attribute.StringSlice("onecamp.agent.governance_blocked", r.GovernanceBlocks))
	}

	_, span := otel.Tracer(tracerName).Start(ctx, "invoke_agent",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(r.StartedAt),
		trace.WithAttributes(attrs...),
	)

	// A blocked run is NOT an error. Governance refusing something is the system
	// working, and marking it Error would page somebody for a success. Nor is a
	// stopped run: a human pressed stop, and that is an outcome, not a fault.
	if r.Status == model.RunFailed {
		span.SetStatus(codes.Error, helpers.FirstNonBlank(r.Error, "the run failed"))
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End(trace.WithTimestamp(endedAt))
}
