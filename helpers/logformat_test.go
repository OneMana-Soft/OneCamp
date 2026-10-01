package helpers

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// captureHandler records what a sink actually receives, which is the only thing that matters
// here: the defect was invisible at the call site and only showed up in the emitted record.
type captureHandler struct {
	records []slog.Record
	attrs   []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	r.Attrs(func(a slog.Attr) bool {
		h.attrs = append(h.attrs, a)
		return true
	})
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// withCapturedLogger swaps the package logger for one that records, and restores it.
func withCapturedLogger(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := Logger
	Logger = slog.New(h)
	t.Cleanup(func() { Logger = prev })
	return h
}

func (h *captureHandler) attr(name string) (slog.Value, bool) {
	for _, a := range h.attrs {
		if a.Key == name {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

func TestHasFormatVerb(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"failed: %+v", true},
		{"user %s did %d things", true},
		{"%v", true},
		{"nothing to render", false},
		{"", false},
		// An escaped percent is a literal, not a verb. Sprintf on this with an arg beside it
		// would append %!(EXTRA ...) to a message that was already correct.
		{"disk 100%% full", false},
		{"%%", false},
		// A trailing percent introduces nothing; Sprintf would produce %!(NOVERB).
		{"discount is 50%", false},
		// An escaped pair followed by a real verb is still a verb.
		{"100%% of %d failed", true},
	}

	for _, c := range cases {
		if got := hasFormatVerb(c.in); got != c.want {
			t.Errorf("hasFormatVerb(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The house idiom. Roughly 1,360 call sites write this, and every one of them used to emit the
// message with a literal %+v in it — including into the OTel collector, where a body containing
// "%+v" cannot be grouped or searched on.
func TestLogMessageWithVerbsIsRendered(t *testing.T) {
	h := withCapturedLogger(t)

	LogErrorWithContext(context.Background(),
		"business/CreateChannel failed err: %+v", context.DeadlineExceeded)

	if len(h.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(h.records))
	}
	got := h.records[0].Message

	want := "business/CreateChannel failed err: context deadline exceeded"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	// And the value must not ALSO be duplicated into an args attribute.
	if _, ok := h.attr("args"); ok {
		t.Error("the value was rendered into the message AND repeated in args")
	}
}

// Multiple args must land in their own placeholders, not be concatenated. The old path used
// fmt.Sprint(args...), which glues adjacent operands together with no separator — that is how
// "create channel row" and "delete refused" became "create channel rowdelete refused".
func TestLogRendersEveryArgIntoItsOwnPlaceholder(t *testing.T) {
	h := withCapturedLogger(t)

	LogInfoWithContext(context.Background(), "channel %s has %d members", "general", 42)

	if got, want := h.records[0].Message, "channel general has 42 members"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// A site that passes args without a template must keep its args attribute rather than be
// mangled, which is what makes this change safe at every call site without editing any of them.
func TestLogWithoutVerbsKeepsArgsAttribute(t *testing.T) {
	h := withCapturedLogger(t)

	LogWarnWithContext(context.Background(), "cache rebuilt", "channels")

	if got, want := h.records[0].Message, "cache rebuilt"; got != want {
		t.Errorf("message = %q, want %q — a message with no verbs must be left alone", got, want)
	}
	v, ok := h.attr("args")
	if !ok {
		t.Fatal("args attribute was dropped; the value would be lost entirely")
	}
	if v.String() != "channels" {
		t.Errorf("args = %q, want %q", v.String(), "channels")
	}
}

// No args at all must not touch the message, even when it contains a percent — a log line that
// happens to mention "50% of runs" must not become "50%!(NOVERB)".
func TestLogWithNoArgsIsNeverFormatted(t *testing.T) {
	h := withCapturedLogger(t)

	LogInfoWithContext(context.Background(), "50% of runs completed")

	if got, want := h.records[0].Message, "50% of runs completed"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// Trace correlation on the stdout path. The OTel bridge stamps these onto its own record
// natively, but the JSON handler is plain slog and added nothing — so container logs, the first
// thing anyone opens during an incident, could not be tied back to a trace.
func TestLogCarriesTraceCorrelation(t *testing.T) {
	h := withCapturedLogger(t)

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("trace id: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("span id: %v", err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
		Remote:  true,
	}))

	LogErrorWithContext(ctx, "something failed")

	v, ok := h.attr("trace_id")
	if !ok {
		t.Fatal("no trace_id attribute; stdout logs cannot be correlated to a trace")
	}
	if v.String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %q, want the span context's id", v.String())
	}
	if v, ok := h.attr("span_id"); !ok || v.String() != "00f067aa0ba902b7" {
		t.Errorf("span_id = %v (present=%v), want 00f067aa0ba902b7", v.String(), ok)
	}
}

// Without a span there is nothing to correlate, and emitting empty or all-zero ids would be
// worse than emitting none — they look like a real trace that cannot be found.
func TestLogOmitsTraceCorrelationWhenThereIsNoSpan(t *testing.T) {
	h := withCapturedLogger(t)

	LogInfoWithContext(context.Background(), "no span here")

	if _, ok := h.attr("trace_id"); ok {
		t.Error("trace_id was emitted without a span; an all-zero id looks like a findable trace")
	}
	if _, ok := h.attr("span_id"); ok {
		t.Error("span_id was emitted without a span")
	}
}
