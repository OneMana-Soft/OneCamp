//go:build integration
// +build integration

package integration_test

// Events in global search (gcal-13), against a real OpenSearch mapped as the
// server maps it, Postgres 12 and Dgraph, through the calendar's own write
// path, its resource events and the outbox. The people are the references'
// world (work_graph_refs_test.go): olu, mia and out, in one workspace.
//
//   - olu's "Quarterly roadmap review" has mia as a guest; his "Salary talk"
//     is private; out is on neither.
//
// The review's organiser and its guest find it by its title, its notes and
// where it is, mia through an API token too; out finds nothing and counts
// nothing until olu shares his calendar's details with him, and then the
// review alone, never the private talk. No hit carries the notes, who is on
// the event, or an address. The trash takes the review out of everyone's
// search, and the nightly pass writes back an entry the index lost.
//
// Run: integ_rel.sh <wt> <log> '^TestCalendarEventsInGlobalSearch$'

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	globalSearchBusiness "github.com/akashc777/OneCamp/business/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	calendarModels "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/searchprobe"
)

func TestCalendarEventsInGlobalSearch(t *testing.T) {
	w := newRefsWorld(t)
	integration.MarkCalendarMoved(t, w.env.PG)
	search := integration.SetupOpenSearch(t)
	// Every index, mapped as the server maps them at boot.
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}
	ctx := context.Background()
	user := func(name string) *userModels.UserInfo {
		u := w.as(name).Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		return &u
	}
	// settle waits for the resource events' sinks and the outbox, then has
	// the events index show what was written.
	settle := func(t *testing.T) {
		t.Helper()
		helpers.SettleBackground(30 * time.Second)
		w.settleOutbox()
		if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.EVENT_INDEX); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	create := func(in calendarBusiness.EventInput) string {
		t.Helper()
		in.Start, in.End = calendarBusiness.When{At: &start, Zone: "UTC"}, calendarBusiness.When{At: &end, Zone: "UTC"}
		made, err := calendarBusiness.Events.Create(w.as("olu"), user("olu"), in)
		if err != nil {
			t.Fatal(err)
		}
		return made.ID
	}
	review := create(calendarBusiness.EventInput{Title: "Quarterly roadmap review", Notes: "Bring the pricing notes", Location: "Room 4",
		Attendees: []calendarBusiness.GuestInput{{UUID: w.people["mia"].String()}}})
	talk := create(calendarBusiness.EventInput{Title: "Salary talk", Visibility: "private"})
	settle(t)

	// finds is the events a search for text finds as who ("tok" is mia
	// through an API token), by id; every count is what the page shows.
	finds := func(t *testing.T, who, text string) map[string]*openSearchStruct.OpenSearchEvent {
		t.Helper()
		u := user(who)
		subject := accessBusiness.SubjectOf(u)
		if who == "tok" {
			subject = accessBusiness.TokenSubjectOf(u, "")
		}
		p, err := globalSearchBusiness.GetUnifiedGlobalSearch(w.as(who), subject, u.UserPostgresInfo.EmailID, nil, nil, nil, text)
		if err != nil {
			t.Fatalf("%s searching %q: %v", who, text, err)
		}
		countsOnlyWhatShows(t, who+" searching "+text, p)
		out := map[string]*openSearchStruct.OpenSearchEvent{}
		for _, h := range p.Page {
			if h.Event != nil {
				out[h.Event.Uuid] = h.Event
				if raw, _ := json.Marshal(h); strings.Contains(string(raw), "@") {
					t.Errorf("%s searching %q: a hit carries an address: %s", who, text, raw)
				}
			}
		}
		if p.Totals[globalSearchBusiness.KindEvent] != len(out) {
			t.Errorf("%s searching %q: %d events counted, %d shown", who, text, p.Totals[globalSearchBusiness.KindEvent], len(out))
		}
		return out
	}

	t.Run("its organiser and its guest find it by its title, notes and place, and no hit says who is on it", func(t *testing.T) {
		for _, who := range []string{"olu", "mia", "tok"} {
			for _, text := range []string{"roadmap", "pricing notes", "Room 4"} {
				e := finds(t, who, text)[review]
				if e == nil || e.EventTitle != "Quarterly roadmap review" || e.EventLocation != "Room 4" || e.EventStart != start.Unix() {
					t.Errorf("%s searching %q: the review is %+v", who, text, e)
					continue
				}
				if e.EventNotes != "" || len(e.EventPeople) != 0 || len(e.EventGuestNames) != 0 {
					t.Errorf("%s searching %q: a hit carries its notes or people: %+v", who, text, e)
				}
			}
		}
	})

	t.Run("someone neither invited nor shown the calendar finds nothing and counts nothing", func(t *testing.T) {
		for _, text := range []string{"roadmap", "pricing notes", "Room 4", "salary"} {
			if got := finds(t, "out", text); len(got) != 0 {
				t.Errorf("out searching %q found %v", text, got)
			}
		}
		if got := finds(t, "mia", "salary"); len(got) != 0 {
			t.Errorf("mia found olu's private talk: %v", got)
		}
		if got := finds(t, "olu", "salary"); got[talk] == nil {
			t.Errorf("olu can't find his own private talk: %v", got)
		}
	})

	// CR-67 MF1: one page of one hit, where every hit is the post-filter's to
	// drop, so a leak in the query's reach (its counts and its facet) can't
	// hide behind a page that drops what it shows.
	t.Run("out counts none of two events he may not open, a hit a page", func(t *testing.T) {
		create(calendarBusiness.EventInput{Title: "Budget sync for the board"})
		create(calendarBusiness.EventInput{Title: "Budget sync for the bank"})
		settle(t)
		u := user("out")
		reach := globalSearchBusiness.ReachOf(accessBusiness.SubjectOf(u), u.UserPostgresInfo.EmailID, nil, nil, nil)
		for name, kinds := range map[string][]string{"every kind": nil, "events": {globalSearchBusiness.KindEvent}} {
			got := searchprobe.SearchAs(t, w.as("out"), reach, "budget sync", globalSearchBusiness.Page{Size: 1, Kinds: kinds})
			if got.Total != 0 || got.Totals[globalSearchBusiness.KindEvent] != 0 || got.HasMore {
				t.Errorf("%s: out counts total %d, events %d, more %v", name, got.Total, got.Totals[globalSearchBusiness.KindEvent], got.HasMore)
			}
		}
	})

	t.Run("shown olu's details, out finds the review, and never the private talk", func(t *testing.T) {
		list, err := calendarBusiness.Events.Calendars(w.as("olu"), user("olu"))
		if err != nil {
			t.Fatal(err)
		}
		personal := ""
		for _, c := range list {
			if c.Mine && c.Kind == "personal" {
				personal = c.ID
			}
		}
		if err := calendarBusiness.Events.Share(w.as("olu"), user("olu"), personal, w.people["out"].String(), calendarModels.ShareDetails); err != nil {
			t.Fatal(err)
		}
		if got := finds(t, "out", "roadmap"); got[review] == nil || len(got) != 1 {
			t.Errorf("out, shown the details, found %v", got)
		}
		if got := finds(t, "out", "salary"); len(got) != 0 {
			t.Errorf("out found the private talk: %v", got)
		}
	})

	t.Run("the trash takes it out of everyone's search", func(t *testing.T) {
		if err := calendarBusiness.Events.Delete(w.as("olu"), user("olu"), review, "", nil); err != nil {
			t.Fatal(err)
		}
		settle(t)
		for _, who := range []string{"olu", "mia", "out"} {
			if got := finds(t, who, "roadmap"); len(got) != 0 {
				t.Errorf("%s still finds the trashed review: %v", who, got)
			}
		}
	})

	t.Run("the nightly pass writes back an entry the index lost", func(t *testing.T) {
		if err := opensearchInit.DeleteDocument(ctx, openSearchStruct.EVENT_INDEX, talk); err != nil {
			t.Fatal(err)
		}
		settle(t)
		if got := finds(t, "olu", "salary"); len(got) != 0 {
			t.Fatalf("the lost entry is still found: %v", got)
		}
		if err := calendarBusiness.WriteEverySearchEntry(ctx); err != nil {
			t.Fatal(err)
		}
		settle(t)
		if got := finds(t, "olu", "salary"); got[talk] == nil {
			t.Errorf("after the pass, olu can't find his talk: %v", got)
		}
		if got := finds(t, "olu", "roadmap"); len(got) != 0 {
			t.Errorf("the pass brought the trashed review back: %v", got)
		}
	})
}
