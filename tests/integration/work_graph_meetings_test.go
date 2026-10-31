//go:build integration
// +build integration

package integration_test

// Meetings in the work graph (DG12's thin nodes, DG13's event kind) through
// the calendar's own write path, its resource events, the outbox and the
// Connected panel's handler, against Postgres 12 and Dgraph with the
// production schema. The people and Plan are the references' world
// (work_graph_refs_test.go):
//
//   - olu organises "Weekly sync" with mia; out is in the workspace and not
//     on it; its notes link Plan.
//
// Once the outbox has run, Plan's panel names the meeting for mia and never
// for out, and the meeting's own panel names who organised it, who it's with
// and what it links. An edit that takes the link out takes the edge out, and
// the trash takes the meeting off. A node dropped from the graph (a restore)
// comes back with the nightly pass, and a second pass finds no drift.
//
// Run: integ_rel.sh <wt> <log> '^TestWorkGraphMeetings$'

import (
	"context"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	workGraphDomain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// meetingKinds is what a panel asks for to see meetings (the web's default
// kinds don't name them until the web shows them).
const meetingKinds = "doc,task,project,person,event"

func TestWorkGraphMeetings(t *testing.T) {
	w := newRefsWorld(t)
	integration.MarkCalendarMoved(t, w.env.PG)
	ctx := context.Background()
	user := func(name string) *userModels.UserInfo {
		u := w.as(name).Value(helpers.UserInfoContextKey).(userModels.UserInfo)
		return &u
	}
	// settle waits for the resource events' sinks, then for the outbox.
	settle := func() {
		t.Helper()
		helpers.SettleBackground(30 * time.Second)
		w.settleOutbox()
	}
	link := func(key string) string { return "Agenda: https://onecamp.example/app/doc/" + w.id[key] }
	start := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	sync, err := calendarBusiness.Events.Create(w.as("olu"), user("olu"), calendarBusiness.EventInput{
		Title: "Weekly sync", Notes: link("plan"),
		Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"},
		Attendees: []calendarBusiness.GuestInput{{UUID: w.people["mia"].String()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.id["sync"], w.name[sync.ID] = sync.ID, "sync"
	settle()
	panel := func(person, kind, key string) []string {
		t.Helper()
		got, _ := w.connectedKinds(person, kind, key, meetingKinds)
		return got
	}

	t.Run("a meeting whose notes link a doc shows in the doc's panel, to its guest only", func(t *testing.T) {
		if got := panel("mia", "doc", "plan"); !refsHas(got, "event sync via linked_from>sync") {
			t.Errorf("mia's panel on Plan: %v", got)
		}
		if got := panel("out", "doc", "plan"); refsHas(got, "event sync via linked_from>sync") {
			t.Errorf("out sees the meeting: %v", got)
		}
	})

	t.Run("the meeting's panel names its organiser, its guest and what it links", func(t *testing.T) {
		got := panel("mia", "event", "sync")
		for _, want := range []string{"person olu via organised_by>olu", "person mia via with>mia", "doc plan via links_to>plan"} {
			if !refsHas(got, want) {
				t.Errorf("the meeting's panel lacks %q: %v", want, got)
			}
		}
	})

	t.Run("a dropped node comes back with the nightly pass, and a second finds no drift", func(t *testing.T) {
		if _, err := dgexec.Mutate(ctx, `query { n as var(func: eq(cal_event_uuid, "`+sync.ID+`")) }`, nil, dgexec.Mutation{DelNQuads: "uid(n) * * ."}); err != nil {
			t.Fatal(err)
		}
		if got := panel("mia", "doc", "plan"); refsHas(got, "event sync via linked_from>sync") {
			t.Fatalf("the dropped node is still read: %v", got)
		}
		reps, err := refsBusiness.ReconcileAll(ctx, refsBusiness.Options{Apply: true, Kinds: []workGraphDomain.Kind{workGraphDomain.KindEvent}})
		if err != nil || len(reps) != 1 || reps[0].Drifted != 1 {
			t.Fatalf("the rebuild: %+v (%v)", reps, err)
		}
		if got := panel("mia", "doc", "plan"); !refsHas(got, "event sync via linked_from>sync") {
			t.Errorf("after the rebuild: %v", got)
		}
		reps, err = refsBusiness.ReconcileAll(ctx, refsBusiness.Options{Apply: true, Kinds: []workGraphDomain.Kind{workGraphDomain.KindEvent}})
		if err != nil || reps[0].Drifted != 0 {
			t.Errorf("a second pass: %+v (%v)", reps, err)
		}
	})

	// edit changes the meeting's notes as its organiser does, at the version
	// last read.
	seq := sync.Sequence
	edit := func(notes string) {
		t.Helper()
		got, err := calendarBusiness.Events.Update(w.as("olu"), user("olu"), sync.ID, calendarBusiness.EventPatch{Sequence: &seq, Notes: &notes})
		if err != nil {
			t.Fatal(err)
		}
		seq = got.Sequence
	}

	t.Run("an edit that takes the link out takes the edge out", func(t *testing.T) {
		edit("Agenda to follow")
		settle()
		if got := panel("mia", "doc", "plan"); refsHas(got, "event sync via linked_from>sync") {
			t.Errorf("the edge outlived its link: %v", got)
		}
	})

	t.Run("a meeting in the trash leaves every panel", func(t *testing.T) {
		edit(link("plan"))
		settle()
		if got := panel("mia", "doc", "plan"); !refsHas(got, "event sync via linked_from>sync") {
			t.Fatalf("the link put back: %v", got)
		}
		if err := calendarBusiness.Events.Delete(w.as("olu"), user("olu"), sync.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		settle()
		for _, who := range []string{"olu", "mia"} {
			if got := panel(who, "doc", "plan"); refsHas(got, "event sync via linked_from>sync") {
				t.Errorf("%s still sees the trashed meeting: %v", who, got)
			}
		}
	})
}
