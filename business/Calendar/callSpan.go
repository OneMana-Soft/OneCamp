package Calendar

import (
	"context"
	"slices"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// callLead is how long before an instance starts a call opened for it may
// begin: a call opened a few minutes early is that meeting's, not the one
// before's.
const callLead = 15 * time.Minute

// callNeighbourReach is how far after an instance callSpanOf looks for the
// next one (a yearly series' next is a year on), and callNeighbourLimit how
// many instances each look reads past the series' skipped dates.
const (
	callNeighbourReach = 10 // years
	callNeighbourLimit = 8
)

// CallSpan is the stretch of time [From, To) in which the calls of one
// instance of a series start: a zero From reaches back to the start of time
// (the series' first instance), a zero To on for ever (its last).
type CallSpan struct{ From, To time.Time }

// instanceCallSpan is the CallSpan of an instance that starts at start, given
// whether it is its series' first and the next instance's start (nil after the
// last): from callLead before it starts to callLead before the next one does.
// The spans of a series tile time, so every call is exactly one instance's:
// the one whose start, less callLead, it last followed, or the first's when it
// came before them all. A future instance's span is still to come, so it holds
// no call until one is held in it.
func instanceCallSpan(start time.Time, first bool, next *time.Time) CallSpan {
	var span CallSpan
	if !first {
		span.From = start.Add(-callLead)
	}
	if next != nil {
		span.To = next.Add(-callLead)
	}
	return span
}

// Holds is whether a call that started at started and ended at ended is one of
// the keys' event's: every call filed for a single event or a series' own id
// (no Span), and for one instance only a call that started in its Span. A
// call with no start (a transcript-only node the boot repair ended, which
// writes none) is placed by its end, as domain/Recording's
// EventCallArtefactsStarted reads it; one with neither is its series' alone.
func (k CallKeys) Holds(started, ended *time.Time) bool {
	if k.Span == nil {
		return true
	}
	at := started
	if at == nil {
		at = ended
	}
	if at == nil {
		return false
	}
	t := *at
	return (k.Span.From.IsZero() || !t.Before(k.Span.From)) && (k.Span.To.IsZero() || t.Before(k.Span.To))
}

// callSpanOf is the CallSpan of inst, one instance of a series, with its
// neighbours placed by the series' own expander (instancesOf): skipped dates,
// cancelled instances and moved ones as every view sees them, across every row
// that shares the series' call (callRows), so the halves of a split series
// tile time together.
func (s *service) callSpanOf(ctx context.Context, inst instance) (*CallSpan, error) {
	start, _ := spanOf(inst.Event)
	rows, err := s.callRows(ctx, inst.series)
	if err != nil {
		return nil, err
	}
	limit, earliest := callNeighbourLimit, start
	for _, e := range rows {
		limit += len(e.Exdates)
		if at, _, _ := seriesTime(e); at.Before(earliest) {
			earliest = at
		}
	}
	startsOf := func(w models.Window) ([]time.Time, error) {
		insts, err := s.instancesOf(ctx, rows, w, limit)
		var out []time.Time
		for _, i := range insts {
			at, _ := spanOf(i.Event)
			out = append(out, at)
		}
		return out, err
	}
	// Back to before the earliest row starts, as far as the next is looked
	// for ahead, so an instance moved before its series' start is found: no
	// rule date lies there, so the read gains only such moved instances.
	before, err := startsOf(spanWindow(earliest.AddDate(-callNeighbourReach, 0, 0), start))
	if err != nil {
		return nil, err
	}
	after, err := startsOf(spanWindow(start, start.AddDate(callNeighbourReach, 0, 0)))
	if err != nil {
		return nil, err
	}
	first := !slices.ContainsFunc(before, func(at time.Time) bool { return at.Before(start) })
	var next *time.Time
	for _, at := range after {
		if at.After(start) && (next == nil || at.Before(*next)) {
			next = &at
		}
	}
	span := instanceCallSpan(start, first, next)
	return &span, nil
}

// callRows is the rows whose instances hold series' call: series, and every
// other live row in its call room, which is the part a "this and following"
// split made (cloneEvent keeps CallRoom; models.HoldingRoom).
func (s *service) callRows(ctx context.Context, series *models.Event) ([]*models.Event, error) {
	rows := []*models.Event{series}
	if series.CallRoom == "" {
		return rows, nil
	}
	held, err := s.store.holdingRoom(ctx, series.CallRoom)
	if err != nil {
		return nil, err
	}
	for _, e := range held {
		if e.ID != series.ID {
			rows = append(rows, e)
		}
	}
	return rows, nil
}
