package business

import (
	"context"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The workload's time off goes through the calendar's one rule with the
// asker (R-WORKLOAD): someone who keeps when they're busy to themselves, or
// anyone's while the workspace keeps it, adds no time off to the asker's
// workload (their capacity reads as whole), from the calendar's store and
// from the graph's away events alike; the asker's own always shows; and
// nobody's time off is read when whose shows can't be decided.
func TestWorkloadTimeOffShowsOnlyWhoseBusyTimesTheAskerMaySee(t *testing.T) {
	ctx := context.Background()
	me, alice, bob := "a0000000-0000-0000-0000-000000000001", "a0000000-0000-0000-0000-000000000002", "a0000000-0000-0000-0000-000000000003"
	from, until := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	start, end := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	prevSeen, prevAway := peopleSeenBy, awaySpans
	t.Cleanup(func() { peopleSeenBy, awaySpans = prevSeen, prevAway })
	var asker string
	peopleSeenBy = func(_ context.Context, a string, people []string) ([]string, []string, error) {
		asker = a
		var seen, private []string
		for _, p := range people {
			if p == bob {
				private = append(private, p)
			} else {
				seen = append(seen, p)
			}
		}
		return seen, private, nil
	}
	var asked []string
	awaySpans = func(_ context.Context, people []string, _, _ time.Time) ([]calendarBusiness.PersonSpan, error) {
		asked = people
		return []calendarBusiness.PersonSpan{{Person: me, Start: start, End: end}, {Person: alice, Start: start, End: end}, {Person: bob, Start: start, End: end}}, nil
	}
	people := []string{me, alice, bob}
	owners := func(spans []AwaySpan) map[string]bool {
		out := map[string]bool{}
		for _, s := range spans {
			out[s.UserUUID] = true
		}
		return out
	}

	got, err := awayShown(ctx, me, people, from, until, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := owners(got); !o[me] || !o[alice] || o[bob] || asker != me {
		t.Fatalf("from the store, for me: %v (asked as %q)", o, asker)
	}
	for _, p := range asked {
		if p == bob {
			t.Fatalf("Bob's time off was read though he keeps it: %v", asked)
		}
	}

	graph := []*dgraphStruct.DgraphEvent{}
	for _, p := range people {
		s, e := start, end
		graph = append(graph, &dgraphStruct.DgraphEvent{CreatedBy: &dgraphStruct.DgraphUser{Uuid: p}, StartTime: &s, EndTime: &e})
	}
	got, err = awayShown(ctx, me, people, from, until, false, graph)
	if err != nil {
		t.Fatal(err)
	}
	if o := owners(got); !o[me] || !o[alice] || o[bob] {
		t.Fatalf("from the graph, for me: %v", o)
	}

	peopleSeenBy = func(context.Context, string, []string) ([]string, []string, error) {
		return nil, nil, context.DeadlineExceeded
	}
	if got, err := awayShown(ctx, me, people, from, until, true, nil); err == nil {
		t.Fatalf("whose busy times show couldn't be read, yet time off came back: %+v", got)
	}
}
