//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// Calendar head 5b (gcal-9b) on Postgres: an out-of-office with a rule keeps
// it (event_ooo_rules, migration 932), reads it back to its owner alone, and
// declines a later invitation in its time with its message as the note.
func TestOutOfOfficeDeclinesInPostgres(t *testing.T) {
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	ctx := context.Background()
	from := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	to := from.Add(72 * time.Hour)

	away, err := calendarBusiness.Events.Create(ctx, maya, calendarBusiness.EventInput{Kind: calendarBusiness.EventAway, Title: "Out of office",
		Start: calendarBusiness.When{At: &from, Zone: "UTC"}, End: calendarBusiness.When{At: &to},
		OutOfOffice: &calendarBusiness.OOOInput{Decline: "new", Message: "Back Monday."}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := calendarBusiness.Events.Get(ctx, maya, away.ID)
	if err != nil || got.OutOfOffice == nil || got.OutOfOffice.Decline != "new" || got.OutOfOffice.Message != "Back Monday." {
		t.Fatalf("Maya reads her rule back: %+v %v", got, err)
	}
	if seen, err := calendarBusiness.Events.Get(ctx, sam, away.ID); err == nil && seen.OutOfOffice != nil {
		t.Fatal("Sam never reads Maya's rule")
	}

	st := from.Add(26 * time.Hour)
	en := st.Add(30 * time.Minute)
	sync, err := calendarBusiness.Events.Create(ctx, sam, calendarBusiness.EventInput{Title: "Sync",
		Start: calendarBusiness.When{At: &st, Zone: "UTC"}, End: calendarBusiness.When{At: &en},
		Attendees: []calendarBusiness.GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	var response, note string
	if err := env.PG.QueryRow(`SELECT response, note FROM event_attendees WHERE event_id = $1 AND user_id = $2`,
		sync.ID, maya.UserPostgresInfo.Id).Scan(&response, &note); err != nil {
		t.Fatal(err)
	}
	if response != "declined" || note != "Back Monday." {
		t.Fatalf("Maya's answer to Sam's sync: %q %q, want declined with her message", response, note)
	}

	// The rule goes with its event.
	if _, err := env.PG.Exec(`DELETE FROM events WHERE id = $1`, away.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := env.PG.QueryRow(`SELECT count(*) FROM event_ooo_rules WHERE event_id = $1`, away.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rule left after its event: %d %v", left, err)
	}
}
