//go:build integration
// +build integration

package integration_test

// Proposing another time (calendar head 4, gcal-4) in the one store, against
// real Postgres 12: the migration up, down and up, a proposal whole or not at
// all; and over HTTP, a guest's proposal seen by the organiser and no other
// guest, set aside when the event moves, and dismissed only by whoever may
// change the event. The service's own rules are pinned in
// business/Calendar/proposals_test.go; these hold the SQL to them.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarProposals' -v

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/tests/integration"
)

// proposalsMigration is head 4's migration in one direction, found by name:
// the integrator numbers it at merge.
func proposalsMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_event_proposals."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one event_proposals %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The migration goes up, down and up again on Postgres 12, and a proposal is
// whole (its time and the time it was proposed against) or absent.
func TestCalendarProposalsMigrationUpDownUp(t *testing.T) {
	up, down := proposalsMigration(t, "up"), proposalsMigration(t, "down")
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, table := range []string{"event_attendees", "event_instance_answers"} {
			for _, c := range []string{"proposed_start", "proposed_end", "proposed_against_start", "proposed_against_end"} {
				if got := columnExists(t, env, table, c); got != step.want {
					t.Fatalf("step %d: %s.%s exists = %v", i, table, c, got)
				}
			}
		}
	}
	user, calendar, event := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) error { _, err := env.PG.Exec(q, args...); return err }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'u', 'u', NOW(), NOW())`, user, user.String()+"@example.test"))
	must(exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, user))
	must(exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour')`,
		event, calendar, user, event.String()+"@onecamp"))
	must(exec(`INSERT INTO event_attendees (event_id, email, role, response, added_at) VALUES ($1, 'alex@example.org', 'required', 'needs_action', NOW())`, event))
	if err := exec(`UPDATE event_attendees SET proposed_start = NOW() WHERE event_id = $1`, event); err == nil {
		t.Error("half a proposal was kept")
	}
	// A CHECK that comes to NULL passes, so no part may be left out: here the start.
	if err := exec(`UPDATE event_attendees SET proposed_end = NOW() + interval '1 hour', proposed_against_start = NOW(), proposed_against_end = NOW()
		WHERE event_id = $1`, event); err == nil {
		t.Error("a proposal with no start was kept")
	}
	if err := exec(`UPDATE event_attendees SET proposed_start = NOW(), proposed_end = NOW() - interval '1 hour', proposed_against_start = NOW(), proposed_against_end = NOW()
		WHERE event_id = $1`, event); err == nil {
		t.Error("a proposal that ends before it starts was kept")
	}
	must(exec(`UPDATE event_attendees SET proposed_start = NOW() + interval '2 hours', proposed_end = NOW() + interval '3 hours',
		proposed_against_start = NOW(), proposed_against_end = NOW() + interval '1 hour' WHERE event_id = $1`, event))
}

func TestCalendarProposalsOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")

	// Tomorrow, from the clock: a proposal is for a time still to come.
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+at(9)+`","zone":"UTC"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+jonas.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	path := "/calendar/events/" + e.ID
	proposal := `"proposal":{"start":"` + at(14) + `","end":"` + at(15) + `"}`
	if code, body := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, path+"/answer", maya, `{"answer":"declined","note":"Clash",`+proposal+`}`, e.ID); code != http.StatusOK {
		t.Fatalf("Maya proposes: %d %v", code, body)
	}
	// Maya's proposal as a reader's read of the event shows it: its start, or nil.
	proposalTo := func(code int, body map[string]json.RawMessage) *time.Time {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("read: %d %v", code, body)
		}
		g := guestOf(dataEvent(t, body), "Maya")
		if g == nil || g.Proposal == nil {
			return nil
		}
		start := g.Proposal.Start
		return &start
	}
	if p := proposalTo(v2(t, eventController.GetCalendarEvent, http.MethodGet, path, sam, "", e.ID)); p == nil || p.Format(time.RFC3339) != at(14) {
		t.Errorf("Maya's proposal, to Sam: %v", p)
	}
	if p := proposalTo(v2(t, eventController.GetCalendarEvent, http.MethodGet, path, maya, "", e.ID)); p == nil {
		t.Error("Maya's proposal, to Maya: none")
	}
	if p := proposalTo(v2(t, eventController.GetCalendarEvent, http.MethodGet, path, jonas, "", e.ID)); p != nil {
		t.Errorf("Maya's proposal, to Jonas: %v", p)
	}

	// Only whoever may change the event dismisses it.
	dismiss := `{"user_uuid":"` + maya.UserDgraphInfo.Uuid + `"}`
	if code, body := v2(t, eventController.DismissCalendarProposal, http.MethodPost, path+"/proposal/dismiss", jonas, dismiss, e.ID); code != http.StatusForbidden {
		t.Errorf("Jonas dismissed it: %d %v", code, body)
	}
	if code, body := v2(t, eventController.DismissCalendarProposal, http.MethodPost, path+"/proposal/dismiss", sam, dismiss, e.ID); code != http.StatusOK {
		t.Fatalf("Sam dismisses it: %d %v", code, body)
	}
	if p := proposalTo(v2(t, eventController.GetCalendarEvent, http.MethodGet, path, sam, "", e.ID)); p != nil {
		t.Errorf("dismissed, still seen: %v", p)
	}

	// Proposed again, then the event moves: the proposal is set aside.
	if code, body := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, path+"/answer", maya, `{"answer":"declined",`+proposal+`}`, e.ID); code != http.StatusOK {
		t.Fatalf("Maya proposes again: %d %v", code, body)
	}
	moved := `{"sequence":` + itoa(e.Sequence) + `,"notify":"none","start":{"at":"` + at(14) + `","zone":"UTC"},"end":{"at":"` + at(15) + `"}}`
	if code, body := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, path, sam, moved, e.ID); code != http.StatusOK {
		t.Fatalf("Sam uses Maya's time: %d %v", code, body)
	}
	if p := proposalTo(v2(t, eventController.GetCalendarEvent, http.MethodGet, path, sam, "", e.ID)); p != nil {
		t.Errorf("the event moved to it, and the proposal still stands: %v", p)
	}
}
