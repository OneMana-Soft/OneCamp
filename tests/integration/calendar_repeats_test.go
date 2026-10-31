//go:build integration
// +build integration

package integration_test

// What repeats on a person's calendar beside their events (calendar head 9,
// gcal-1b), over HTTP against real Postgres and Dgraph: GET /calendar/repeats
// answers the check-ins of the channels in the asker's session (read as the
// sign-in reads it) at each time they ask, and the dates their open tasks
// that repeat on a schedule will be due next. Someone outside the channel
// sees none of its check-ins, and nobody sees another person's tasks: Maya,
// in the workspace but not in #standup, sees only her own task. The rules
// themselves are pinned in business/Calendar repeats_test.go, business/Task
// dueAhead_test.go and business/CheckIn asks_test.go.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestCalendarRepeats' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestCalendarRepeatsAreTheAskersOwn(t *testing.T) {
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	const live = "0001-01-01T00:00:00Z"
	rfc3339 := func(tm time.Time) string { return tm.UTC().Format(time.RFC3339) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// #standup has Sam in it, not Maya. A running check-in asks every day at
	// 17:00 UTC; a paused one asks nothing.
	standup, project := uuid.New(), uuid.New()
	running, paused := uuid.New(), uuid.New()
	// Tasks: Sam's weekly one, due next Monday; Sam's done one and his
	// one that repeats after completion; Maya's weekly one.
	samWeekly, samDone, samAfter, mayaWeekly := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	monday := time.Now().UTC().Truncate(24 * time.Hour)
	for monday.Weekday() != time.Monday || !monday.After(time.Now()) {
		monday = monday.AddDate(0, 0, 1)
	}
	task := func(id uuid.UUID, name, status, who string) map[string]any {
		return map[string]any{"uid": "_:" + id.String(), "dgraph.type": "Task", "task_uuid": id.String(), "task_name": name,
			"task_status": status, "task_deleted_at": live, "task_start_date": live, "task_due_date": rfc3339(monday),
			"task_project":  map[string]any{"uid": "_:P"},
			"task_assignee": map[string]any{"uid": who, "user_tasks": []map[string]any{{"uid": "_:" + id.String()}}}}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": standup.String(), "ch_name": "standup", "ch_deleted_at": live,
			"ch_members": []map[string]any{{"uid": sam.UserDgraphInfo.Uid}}},
		{"uid": sam.UserDgraphInfo.Uid, "user_channels": []map[string]any{{"uid": "_:ch"}}, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": maya.UserDgraphInfo.Uid, "user_projects": []map[string]any{{"uid": "_:P"}}},
		{"uid": "_:P", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Ops", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}},
		task(samWeekly, "Plan the week", "todo", sam.UserDgraphInfo.Uid),
		task(samDone, "Last week's report", "done", sam.UserDgraphInfo.Uid),
		task(samAfter, "Water the plants", "todo", sam.UserDgraphInfo.Uid),
		task(mayaWeekly, "Maya's review", "todo", maya.UserDgraphInfo.Uid),
	})
	// The tasks' rows in Postgres, which a repeat belongs to.
	team := uuid.New()
	_, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Ops', $2)`, team, sam.UserPostgresInfo.Id)
	must(err)
	_, err = env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Ops', $2, $3)`, project, team, sam.UserPostgresInfo.Id)
	must(err)
	_, err = env.PG.Exec(`INSERT INTO tasks (id, project_id, created_by) SELECT unnest($1::uuid[]), $2::uuid, $3::uuid`,
		pq.Array([]string{samWeekly.String(), samDone.String(), samAfter.String(), mayaWeekly.String()}), project, sam.UserPostgresInfo.Id)
	must(err)
	for _, c := range []struct {
		id     uuid.UUID
		paused bool
	}{{running, false}, {paused, true}} {
		_, err := env.PG.Exec(`INSERT INTO checkins (id, channel_uuid, question, days, at_minute, tz, created_by, paused, next_run_at)
			VALUES ($1, $2, 'What did you work on today?', 127, 1020, 'UTC', $3, $4, NOW() + interval '1 hour')`,
			c.id, standup, sam.UserPostgresInfo.Id, c.paused)
		must(err)
	}
	for _, r := range []struct {
		id   uuid.UUID
		mode string
	}{{samWeekly, "schedule"}, {samDone, "schedule"}, {samAfter, "completion"}, {mayaWeekly, "schedule"}} {
		_, err := env.PG.Exec(`INSERT INTO task_recurrences (task_uuid, rule, mode, time_zone, anchor_day, created_by)
			VALUES ($1, 'FREQ=WEEKLY;BYDAY=MO', $2, 'UTC', 0, $3)`, r.id, r.mode, sam.UserPostgresInfo.Id)
		must(err)
	}

	// CR-67 MF1: more repeats on a schedule than the first read's cap
	// (2,000), all changed after Sam's: his still shows.
	_, err = env.PG.Exec(`WITH t AS (INSERT INTO tasks (id, project_id, created_by)
			SELECT uuid_generate_v4(), $1::uuid, $2::uuid FROM generate_series(1, 2001) RETURNING id)
		INSERT INTO task_recurrences (task_uuid, rule, mode, time_zone, anchor_day, created_by, updated_at)
		SELECT id, 'FREQ=DAILY', 'schedule', 'UTC', 0, $2::uuid, NOW() + interval '1 day' FROM t`, project, sam.UserPostgresInfo.Id)
	must(err)

	// Each asks as the sign-in reads them: their channels from the graph.
	session := func(u *userModels.UserInfo) *userModels.UserInfo {
		t.Helper()
		got, err := userDomain.GetActiveDgraphUserInfoByUUID(context.Background(), u.UserDgraphInfo.Uuid)
		if err != nil || got == nil {
			t.Fatalf("session of %s: %v", u.UserDgraphInfo.UserName, err)
		}
		s := *u
		s.UserDgraphInfo = *got
		return &s
	}
	from, to := monday, monday.AddDate(0, 0, 15)
	repeats := func(u *userModels.UserInfo, from, to time.Time) (int, calendarBusiness.RepeatsView) {
		t.Helper()
		code, body := v2(t, eventController.ListCalendarRepeats, http.MethodGet, "/calendar/repeats?from="+rfc3339(from)+"&to="+rfc3339(to), u, "", "")
		var view calendarBusiness.RepeatsView
		if code == http.StatusOK {
			must(json.Unmarshal(body["data"], &view))
		}
		return code, view
	}

	samAsks := session(sam)
	code, view := repeats(samAsks, from, to)
	if code != http.StatusOK {
		t.Fatalf("Sam: %d", code)
	}
	if len(view.CheckIns) != 1 || view.CheckIns[0].ID != running.String() || view.CheckIns[0].ChannelName != "standup" || len(view.CheckIns[0].At) != 15 {
		t.Fatalf("Sam's check-ins: %+v", view.CheckIns)
	}
	if at := view.CheckIns[0].At[0].UTC(); !at.Equal(monday.Add(17 * time.Hour)) {
		t.Fatalf("Sam's first check-in at %v", at)
	}
	if len(view.Tasks) != 1 || view.Tasks[0].TaskUUID != samWeekly.String() || view.Tasks[0].ProjectName != "Ops" || len(view.Tasks[0].Due) != 2 ||
		!view.Tasks[0].Due[0].Equal(monday.AddDate(0, 0, 7)) || !view.Tasks[0].Due[1].Equal(monday.AddDate(0, 0, 14)) {
		t.Fatalf("Sam's tasks ahead: %+v", view.Tasks)
	}

	// Maya: no check-ins from a channel she isn't in, and only her own task.
	code, view = repeats(session(maya), from, to)
	if code != http.StatusOK || len(view.CheckIns) != 0 || len(view.Tasks) != 1 || view.Tasks[0].TaskUUID != mayaWeekly.String() {
		t.Fatalf("Maya: %d %+v", code, view)
	}

	// A stretch it can't read is refused in words.
	for name, span := range map[string][2]time.Time{
		"more than 62 days": {from, from.AddDate(0, 0, 63)},
		"backwards":         {to, from},
	} {
		if code, _ := repeats(samAsks, span[0], span[1]); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
}
