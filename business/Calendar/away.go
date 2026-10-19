package Calendar

// Time off for the workload (business/Project): the out-of-office events of
// the people a workload shows, from the one store. It read every away event
// in the workspace from the graph and kept those of the people shown; this
// reads only theirs, and at most maxInstances of them.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PersonSpan is a stretch of time someone's calendar marks: only its start
// and end leave the calendar, never what the event says.
type PersonSpan struct {
	Person     string
	Start, End time.Time
}

// AwaySpans is these people's time off that touches from to until. An
// all-day day off is the whole of that day in its own zone.
func AwaySpans(ctx context.Context, people []string, from, until time.Time) ([]PersonSpan, error) {
	return stored.awaySpans(ctx, people, from, until)
}

func (s *service) awaySpans(ctx context.Context, people []string, from, until time.Time) ([]PersonSpan, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, p := range people {
		ids = appendUUIDs(ids, p)
	}
	w, err := window(from, until)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.awayOf(ctx, ids, w, maxInstances)
	if err != nil {
		return nil, err
	}
	out := make([]PersonSpan, 0, len(rows))
	for _, e := range rows {
		start, end := spanOf(e)
		if !end.After(start) {
			continue
		}
		out = append(out, PersonSpan{Person: e.CreatedBy.String(), Start: start, End: end})
	}
	return out, nil
}
