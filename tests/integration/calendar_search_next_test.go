//go:build integration
// +build integration

package integration_test

// A repeating event found in global search says when it is next on, never
// its first instance (V-22), against a real OpenSearch mapped as the server
// maps it, Postgres 12 and Dgraph, through the calendar's own write path and
// the outbox. The people are the references' world (work_graph_refs_test.go).
//
//   - olu's "Weekly launch sync" began two weeks ago (less a day), every week
//     for an hour; his "One-off launch retro" doesn't repeat.
//
// The index keeps the sync by its first instance; the search says the next
// one that hasn't ended, then, with that one deleted, the one after. The
// retro keeps its own time.
//
// Run: integ_rel.sh <wt> <log> '^TestSearchShowsARepeatingEventsNextInstance$'

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestSearchShowsARepeatingEventsNextInstance(t *testing.T) {
	w := newRefsWorld(t)
	integration.MarkCalendarMoved(t, w.env.PG)
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	ctx := context.Background()
	u := w.as("olu").Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	olu := &u
	settle := func(t *testing.T) {
		t.Helper()
		helpers.SettleBackground(30 * time.Second)
		w.settleOutbox()
		if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.EVENT_INDEX); err != nil {
			t.Fatal(err)
		}
	}
	// In UTC, so no clock change moves an instance.
	first := time.Now().UTC().Truncate(time.Hour).Add(-13*24*time.Hour + 2*time.Hour)
	next := first.Add(14 * 24 * time.Hour)
	create := func(title string, start time.Time, r *calendarBusiness.Recurrence) string {
		t.Helper()
		end := start.Add(time.Hour)
		made, err := calendarBusiness.Events.Create(w.as("olu"), olu, calendarBusiness.EventInput{Title: title, Recurrence: r,
			Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}})
		if err != nil {
			t.Fatal(err)
		}
		return made.ID
	}
	sync := create("Weekly launch sync", first, &calendarBusiness.Recurrence{Rule: "FREQ=WEEKLY"})
	retroAt := first.Add(3 * 24 * time.Hour)
	retro := create("One-off launch retro", retroAt, nil)
	settle(t)

	finds := func(t *testing.T) map[string]*openSearchStruct.OpenSearchEvent {
		t.Helper()
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(w.as("olu"), accessBusiness.SubjectOf(olu), olu.UserPostgresInfo.EmailID, nil, nil, nil, "launch")
		if err != nil {
			t.Fatalf("searching: %v", err)
		}
		out := map[string]*openSearchStruct.OpenSearchEvent{}
		for _, h := range p.Page {
			if h.Event != nil {
				out[h.Event.Uuid] = h.Event
			}
		}
		return out
	}
	says := func(e *openSearchStruct.OpenSearchEvent) string {
		if e == nil {
			return "nothing"
		}
		return time.Unix(e.EventStart, 0).UTC().Format(time.RFC3339) + " to " + time.Unix(e.EventEnd, 0).UTC().Format(time.RFC3339)
	}
	at := func(s time.Time) string {
		return s.Format(time.RFC3339) + " to " + s.Add(time.Hour).Format(time.RFC3339)
	}

	t.Run("the series says its next instance, the single event its own time", func(t *testing.T) {
		got := finds(t)
		if s := says(got[sync]); s != at(next) {
			t.Errorf("the sync says %s; want its next instance %s (the first was %s)", s, at(next), at(first))
		}
		if s := says(got[retro]); s != at(retroAt) {
			t.Errorf("the retro says %s; want %s", s, at(retroAt))
		}
	})

	t.Run("with the next instance deleted, the one after", func(t *testing.T) {
		from, to := next.Add(-time.Hour), next.Add(2*time.Hour)
		es, _, err := calendarBusiness.Events.Range(w.as("olu"), olu, from, to)
		if err != nil {
			t.Fatal(err)
		}
		var inst *calendarBusiness.Event
		for i := range es {
			if es[i].SeriesID == sync {
				inst = &es[i]
			}
		}
		if inst == nil {
			t.Fatalf("no instance of the sync at %s in %d events", next, len(es))
		}
		seq := inst.Sequence
		if err := calendarBusiness.Events.Delete(w.as("olu"), olu, inst.ID, calendarBusiness.ScopeThis, &seq); err != nil {
			t.Fatal(err)
		}
		settle(t)
		if s, want := says(finds(t)[sync]), at(next.Add(7*24*time.Hour)); s != want {
			t.Errorf("the sync says %s; want %s", s, want)
		}
	})
}
