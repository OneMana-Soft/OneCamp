//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/calendar/v3"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A task synced to Google Calendar has a copy there, and the calendar lists
// the task already, so the copy that comes back from Google is left out. The
// copies were matched on the tasks assigned to the reader, through a column
// nothing writes, so none ever matched and every synced task showed twice.
func TestATaskSyncedToGoogleIsListedOnce(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		env, dg := calendarStores(t)
		sam := seedCalendarPerson(t, env.PG, dg, "Sam")
		olu := seedCalendarPerson(t, env.PG, dg, "Olu")

		launch, synced, gone := uuid.New(), uuid.New(), uuid.New()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Launch', $2)`, []any{launch, olu.UserPostgresInfo.Id}},
			// Synced by Olu, whoever it is assigned to now.
			{`INSERT INTO tasks (id, project_id, created_by, task_google_calendar_id) VALUES ($1, $2, $3, 'g-synced-task')`,
				[]any{synced, launch, olu.UserPostgresInfo.Id}},
			// Deleted: its Google copy is no longer the task's.
			{`INSERT INTO tasks (id, project_id, created_by, task_google_calendar_id, deleted_at) VALUES ($1, $2, $3, 'g-deleted-task', NOW())`,
				[]any{gone, launch, olu.UserPostgresInfo.Id}},
		} {
			if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}

		from := time.Now().UTC().Truncate(time.Hour)
		to := from.Add(48 * time.Hour)
		at := func(id string, h int) *calendar.Event {
			start := from.Add(time.Duration(h) * time.Hour)
			return &calendar.Event{Id: id, Summary: id,
				Start: &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)},
				End:   &calendar.EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)}}
		}
		google := []*calendar.Event{at("g-synced-task", 2), at("g-own-meeting", 3), at("g-deleted-task", 4)}
		restore := calendarBusiness.SwapGoogleEventsFetchForTest(func(context.Context, *userModels.UserInfo, *time.Time, *time.Time) ([]*calendar.Event, error) {
			return google, nil
		})
		t.Cleanup(restore)

		listed := func(t *testing.T) map[string]bool {
			t.Helper()
			events, err := calendarBusiness.GetEventsList(ctx, sam, &from, &to)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, e := range events {
				got[e.Uuid] = true
			}
			return got
		}

		got := listed(t)
		if got["gcal-g-synced-task"] {
			t.Error("the Google copy of a synced task was listed beside the task")
		}
		for _, want := range []string{"gcal-g-own-meeting", "gcal-g-deleted-task"} {
			if !got[want] {
				t.Errorf("%s, an event that is no task's copy, was left out: %v", want, got)
			}
		}

		t.Run("Google answers nothing", func(t *testing.T) {
			google = nil
			for id := range listed(t) {
				if len(id) > 5 && id[:5] == "gcal-" {
					t.Errorf("listed %s with nothing from Google", id)
				}
			}
		})
	})
}
