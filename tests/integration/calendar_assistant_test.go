//go:build integration
// +build integration

package integration_test

// The assistant's calendar against real Postgres and Dgraph, with nobody's
// Google linked, as on the demo: what's on the calendar reads OneCamp's, an
// event the assistant makes lands in OneCamp with the named teammate as its
// guest, and What needs me names today's meeting (CAL-BUG-13); "remind me"
// sets a Later reminder rather than a busy hour (CAL-BUG-6).
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestTheAssistantsCalendar|TestRemindMe' -v

import (
	"context"
	"strings"
	"testing"
	"time"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
)

func toolIn(t *testing.T, name string) ai.ToolExecutor {
	t.Helper()
	run, ok := ai.GetExecutor(name)
	if !ok {
		t.Fatalf("no executor for %s", name)
	}
	return run
}

func TestTheAssistantsCalendarIsOneCamps(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		maya := seedCalendarPerson(t, env.PG, dg, "Maya")
		inUTC := context.WithValue(ctx, ai.LocalizationContextKey, map[string]string{"timezone": "UTC"})

		// A meeting soon, on OneCamp only: nobody has linked Google.
		soon := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Minute)
		if _, err := calendarBusiness.CreateEvent(ctx, sam, calendarAdapter.CreateOrUpdateEventInput{Title: "Launch review", StartTime: rfc(soon), EndTime: rfc(soon.Add(30 * time.Minute))}); err != nil {
			t.Fatal(err)
		}

		msg, _, err := toolIn(t, "calendar_list_events")(inUTC, ai.ProposedAction{ToolName: "calendar_list_events", Params: map[string]string{"days": "2"}}, sam.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg, "Launch review") || strings.Contains(msg, "isn't connected") {
			t.Fatalf("what's on Sam's calendar: %q", msg)
		}

		// "Schedule pricing with Maya": a OneCamp event, Maya its guest.
		when := time.Now().UTC().Add(26 * time.Hour).Truncate(time.Hour)
		msg, data, err := toolIn(t, "calendar_create_event")(inUTC, ai.ProposedAction{ToolName: "calendar_create_event", Params: map[string]string{
			"title": "Pricing sync", "start_time": rfc(when), "attendees": "Maya",
		}}, sam.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		if data["event_uuid"] == "" || !strings.Contains(msg, "with Maya") {
			t.Fatalf("made %v: %q", data, msg)
		}
		from, to := when.Add(-time.Hour), when.Add(time.Hour)
		events, err := calendarBusiness.GetEventsList(ctx, maya, &from, &to)
		if err != nil {
			t.Fatal(err)
		}
		invited := false
		for _, e := range events {
			invited = invited || (e.Uuid == data["event_uuid"] && e.CreatedBy != nil && e.CreatedBy.Uuid == sam.UserDgraphInfo.Uuid && e.Title == "Pricing sync")
		}
		if !invited {
			t.Fatalf("Maya's calendar lacks Sam's event: %+v", events)
		}
		var made int
		if err := env.PG.QueryRow(`SELECT count(*) FROM `+eventsTable()+` WHERE id = $1 AND created_by = $2`, data["event_uuid"], sam.UserPostgresInfo.Id).Scan(&made); err != nil || made != 1 {
			t.Fatalf("the event isn't OneCamp's: %d (%v)", made, err)
		}

		// What needs me names the meeting, Google or not (the daily note lists it).
		if _, err := env.PG.Exec(`UPDATE ai_settings SET enabled = true`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = env.PG.Exec(`UPDATE ai_settings SET enabled = false`) })
		resp, err := aiBusiness.GetWhatNeedsMe(ctx, sam)
		if err != nil || !resp.Enabled {
			t.Fatalf("what needs me: %+v (%v)", resp, err)
		}
		named := false
		for _, it := range resp.Items {
			named = named || (it.Kind == "Upcoming event" && it.Title == "Launch review")
		}
		if !named {
			t.Fatalf("what needs Sam doesn't name the review: %+v", resp.Items)
		}
	})
}

func TestRemindMeSetsALaterReminderNotABusyHour(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		india, err := time.LoadLocation("Asia/Kolkata")
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Add(26 * time.Hour).Truncate(time.Minute)

		msg, data, err := toolIn(t, "set_reminder")(context.WithValue(ctx, ai.LocalizationContextKey, map[string]string{"timezone": "Asia/Kolkata"}),
			ai.ProposedAction{ToolName: "set_reminder", Params: map[string]string{"title": "Call the bank", "start_time": rfc(at)}}, sam.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg, at.In(india).Format("2 Jan, 15:04")) {
			t.Fatalf("answer %q, want the time in India", msg)
		}

		var kind, title string
		var due time.Time
		if err := env.PG.QueryRow(`SELECT item_type, title, remind_at FROM saved_items WHERE user_id = $1 AND id::text = $2`,
			sam.UserPostgresInfo.Id, data["saved_item_id"]).Scan(&kind, &title, &due); err != nil {
			t.Fatalf("no Later item: %v", err)
		}
		if kind != "reminder" || title != "Call the bank" || !due.Equal(at) {
			t.Fatalf("Later item %s %q due %v", kind, title, due)
		}
		var jobs int
		if err := env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE user_uuid = $1 AND job_type = 'saved_item' AND run_at = $2 AND status = 'pending'`,
			sam.UserPostgresInfo.Id, at).Scan(&jobs); err != nil || jobs != 1 {
			t.Fatalf("%d reminders queued to fire (%v)", jobs, err)
		}

		// Nothing on the calendar, and the hour is free to book.
		var events int
		if err := env.PG.QueryRow(`SELECT count(*) FROM `+eventsTable()+` WHERE created_by = $1`, sam.UserPostgresInfo.Id).Scan(&events); err != nil || events != 0 {
			t.Fatalf("%d calendar events (%v)", events, err)
		}
		busy, err := calendarBusiness.BusyFor(ctx, sam.UserDgraphInfo.Uuid, at.Add(-time.Hour), at.Add(time.Hour))
		if err != nil || len(busy.Intervals) != 0 {
			t.Fatalf("busy %v (%v)", busy, err)
		}
	})
}

// eventsTable is the table the calendar keeps events in, for the store a
// test runs with.
func eventsTable() string {
	if helpers.CalendarInPostgres() {
		return "events"
	}
	return "calendar_events"
}
