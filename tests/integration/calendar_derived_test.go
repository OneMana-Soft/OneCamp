//go:build integration

package integration_test

// Calendars that fill themselves (gcal-14 head A (d)), read over HTTP by each
// reader against real Postgres 12, Dgraph and Redis: who sees what.
//
//   - a project's calendar: its members see each task dated in the window
//     and each milestone on its day; never an undated or a deleted task; a
//     restricted task only to those it lets in (not to a member it keeps
//     out); an outsider sees nothing on it;
//   - a channel's calendar: the tasks its live messages link to, each once,
//     to a reader who may read the task (a channel member outside the task's
//     project, or kept out of a restricted one, sees nothing of it); never a
//     link from a deleted message; a private channel's calendar is its
//     members' alone;
//   - a team's calendar: its members' time off as "Away", by the one rule
//     (busySeenOf): each sees their own; another's while the workspace
//     shows busy times and that person hasn't hidden theirs (someone who
//     never chose shows theirs; hiding and showing them again each read back
//     as chosen); an outsider sees nothing. The workspace switch it turns
//     off is put back when it ends.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestContainerCalendarsShowEachReaderWhatTheyMaySee$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestContainerCalendarsShowEachReaderWhatTheyMaySee(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "g14-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)

	// Sam is in everything. Maya is in the launch project, the general
	// channel and the design team, not in payroll nor the private channel.
	// Ken is in nothing. The launch's vault is restricted: Sam made it and
	// runs the project; Maya, a member, isn't let in.
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	ken := seedCalendarPerson(t, env.PG, dg, "Ken")
	const live = "0001-01-01T00:00:00Z"
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)
	date := func(d int) string { return day.AddDate(0, 0, d).Format("2006-01-02") }
	at := func(d, hour int) string {
		return day.AddDate(0, 0, d).Add(time.Duration(hour) * time.Hour).Format(time.RFC3339)
	}
	launch, payroll, general, secret, team := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	dated, undated, binned, salaries, ghost := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	vault := uuid.NewString()
	user := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	task := func(node, id, name, project, due, deleted string) map[string]any {
		n := map[string]any{"uid": "_:" + node, "dgraph.type": "Task", "task_uuid": id, "task_name": name, "task_status": "todo",
			"task_deleted_at": deleted, "task_project": map[string]any{"uid": "_:" + project}, "task_created_by": user(sam)}
		if due != "" {
			n["task_due_date"] = due
		}
		return n
	}
	post := func(node, channel, deleted string, links ...string) map[string]any {
		refs := make([]map[string]any, 0, len(links))
		for _, l := range links {
			refs = append(refs, map[string]any{"uid": "_:" + l})
		}
		return map[string]any{"uid": "_:" + node, "dgraph.type": "Post", "post_uuid": uuid.NewString(), "post_text": "<p>see the task</p>",
			"post_deleted_at": deleted, "post_created_at": at(-1, 9), "post_by": user(sam), "post_channel": map[string]any{"uid": "_:" + channel},
			"refers_to": refs}
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch, "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{user(sam), user(maya)}, "project_admins": []map[string]any{user(sam)}, "project_created_by": user(sam),
			"project_tasks": []map[string]any{{"uid": "_:dated"}, {"uid": "_:undated"}, {"uid": "_:binned"}, {"uid": "_:ghost"}, {"uid": "_:vault"}}},
		task("dated", dated, "Press kit g14", "launch", at(3, 17), live),
		task("undated", undated, "Someday g14", "launch", "", live),
		task("binned", binned, "Binned g14", "launch", at(3, 17), at(-1, 8)),
		task("ghost", ghost, "Ghost link g14", "launch", at(4, 17), live),
		task("vault", vault, "Vault g14", "launch", at(5, 9), live),
		{"uid": "_:payroll", "dgraph.type": "Project", "project_uuid": payroll, "project_name": "Payroll", "project_deleted_at": live,
			"project_members": []map[string]any{user(sam)}, "project_admins": []map[string]any{user(sam)}, "project_created_by": user(sam),
			"project_tasks": []map[string]any{{"uid": "_:salaries"}}},
		task("salaries", salaries, "Salaries g14", "payroll", at(5, 17), live),
		{"uid": "_:general", "dgraph.type": "Channel", "ch_uuid": general, "ch_name": "general-g14", "ch_private": false, "ch_deleted_at": live,
			"ch_members": []map[string]any{user(sam), user(maya)}, "ch_moderators": []map[string]any{user(sam)},
			"ch_posts": []map[string]any{post("p1", "general", live, "dated"), post("p2", "general", live, "dated", "salaries", "vault"),
				post("p3", "general", at(-1, 10), "ghost")}},
		{"uid": "_:secret", "dgraph.type": "Channel", "ch_uuid": secret, "ch_name": "secret-g14", "ch_private": true, "ch_deleted_at": live,
			"ch_members": []map[string]any{user(sam)}, "ch_moderators": []map[string]any{user(sam)},
			"ch_posts": []map[string]any{post("p4", "secret", live, "dated")}},
		{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Design g14", "team_deleted_at": live,
			"team_members": []map[string]any{user(sam), user(maya)}, "team_admins": []map[string]any{user(sam)}},
	})
	for _, c := range []struct {
		id      string
		private bool
	}{{general, false}, {secret, true}} {
		if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, $2, $3, $4)`,
			c.id, "g14-"+c.id[:8], c.private, sam.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
	}
	// The vault's restriction is Postgres's (models/taskread decides on it).
	if _, err := env.PG.Exec(`WITH p AS (INSERT INTO projects (id, project_name, created_by) VALUES ($1::uuid, 'Launch', $3::uuid) RETURNING id),
		t AS (INSERT INTO tasks (id, project_id, created_by) SELECT $2::uuid, id, $3::uuid FROM p RETURNING id, project_id)
		INSERT INTO task_restrictions (task_id, project_id, restricted_by) SELECT id, project_id, $3::uuid FROM t`,
		launch, vault, sam.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`WITH m AS (INSERT INTO milestones (project_uuid, name, target_date, created_by) VALUES ($1, 'Beta g14', $2, $3) RETURNING id)
		INSERT INTO milestone_projects (project_uuid, milestone_id, position) SELECT $1, id, 0 FROM m`, launch, date(6), sam.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}

	// calendarOf is the container's calendar as who opens it, or "" when
	// who may not.
	calendarOf := func(who *userModels.UserInfo, h http.HandlerFunc, kind, param, id string) string {
		t.Helper()
		rec := call(t, h, http.MethodPost, "/calendar/"+kind+"/"+id+"/calendar", who, "", map[string]string{param: id})
		if rec.Code != http.StatusOK {
			return ""
		}
		var body struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.ID == "" {
			t.Fatalf("%s's calendar: %d %s", kind, rec.Code, rec.Body.String())
		}
		return body.Data.ID
	}
	// derived is what who sees on calendar that no one made on it, by
	// "<source kind>:<source id>", failing on any it sees twice.
	derived := func(who *userModels.UserInfo, calendar string) map[string]calendarBusiness.Event {
		t.Helper()
		q := url.Values{"ids": {calendar}, "from": {at(-1, 0)}, "to": {at(30, 0)}}
		rec := call(t, eventController.ListEventsOnCalendars, http.MethodGet, "/calendar/calendars/events?"+q.Encode(), who, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s reads %s: %d %s", who.UserDgraphInfo.UserName, calendar, rec.Code, rec.Body.String())
		}
		var body struct {
			Data []calendarBusiness.Event `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]calendarBusiness.Event{}
		for _, e := range body.Data {
			if e.Source == nil {
				continue
			}
			key := e.Source.Kind + ":" + e.Source.ID
			if _, twice := out[key]; twice {
				t.Errorf("%s sees %s twice on %s", who.UserDgraphInfo.UserName, key, calendar)
			}
			if !e.ReadOnly || e.CalendarID != calendar || e.ID == "" {
				t.Errorf("%s's %s as derived: %+v", who.UserDgraphInfo.UserName, key, e)
			}
			out[key] = e
		}
		return out
	}
	keys := func(m map[string]calendarBusiness.Event) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return strings.Join(out, " ")
	}
	is := func(t *testing.T, what string, got map[string]calendarBusiness.Event, want ...string) {
		t.Helper()
		ok := len(got) == len(want)
		for _, w := range want {
			if _, has := got[w]; !has {
				ok = false
			}
		}
		if !ok {
			t.Errorf("%s: sees %q, want %q", what, keys(got), strings.Join(want, " "))
		}
	}

	t.Run("a project's calendar: its dated tasks and milestones, to its members", func(t *testing.T) {
		cal := calendarOf(sam, eventController.ProjectCalendar, "projects", "projectId", launch)
		if cal == "" {
			t.Fatal("Sam can't open his project's calendar")
		}
		var beta string
		if err := env.PG.QueryRow(`SELECT id::text FROM milestones WHERE project_uuid = $1`, launch).Scan(&beta); err != nil {
			t.Fatal(err)
		}
		// The restricted vault shows to Sam, whom it lets in, never to Maya.
		for who, want := range map[*userModels.UserInfo][]string{
			sam:  {"task:" + dated, "task:" + ghost, "task:" + vault, "milestone:" + beta},
			maya: {"task:" + dated, "task:" + ghost, "milestone:" + beta},
		} {
			got := derived(who, cal)
			is(t, who.UserDgraphInfo.UserName+" on the project", got, want...)
			if e := got["task:"+dated]; e.Title != "Press kit g14" || !e.AllDay || e.Start.Date != date(3) || e.End.Date != date(4) {
				t.Errorf("the dated task as %s sees it: %+v", who.UserDgraphInfo.UserName, e)
			}
			if e := got["milestone:"+beta]; e.Title != "Beta g14" || e.Start.Date != date(6) || e.End.Date != date(7) {
				t.Errorf("the milestone as %s sees it: %+v", who.UserDgraphInfo.UserName, e)
			}
		}
		if calendarOf(ken, eventController.ProjectCalendar, "projects", "projectId", launch) != "" {
			t.Error("Ken, outside the project, opens its calendar")
		}
		is(t, "Ken on the project", derived(ken, cal))
		// No write path takes a derived id.
		code, _ := v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/task:"+dated, sam, `{"title":"Mine"}`, "task:"+dated)
		if code < 400 {
			t.Errorf("a derived entry was changed: %d", code)
		}
	})

	t.Run("a channel's calendar: the tasks its live messages link to, to readers of each", func(t *testing.T) {
		cal := calendarOf(sam, eventController.ChannelCalendar, "channels", "channelId", general)
		if cal == "" {
			t.Fatal("Sam can't open his channel's calendar")
		}
		// Linked twice, shown once; the deleted message's link never.
		is(t, "Sam on general", derived(sam, cal), "task:"+dated, "task:"+salaries, "task:"+vault)
		// Maya isn't in payroll, and the vault keeps her out: she sees
		// nothing of either.
		is(t, "Maya on general", derived(maya, cal), "task:"+dated)
		// Ken is in no project: whatever he may see of the channel, no task.
		is(t, "Ken on general", derived(ken, cal))

		private := calendarOf(sam, eventController.ChannelCalendar, "channels", "channelId", secret)
		if private == "" {
			t.Fatal("Sam can't open his private channel's calendar")
		}
		is(t, "Sam on the private channel", derived(sam, private), "task:"+dated)
		for _, who := range []*userModels.UserInfo{maya, ken} {
			if calendarOf(who, eventController.ChannelCalendar, "channels", "channelId", secret) != "" {
				t.Errorf("%s, outside the private channel, opens its calendar", who.UserDgraphInfo.UserName)
			}
			is(t, who.UserDgraphInfo.UserName+" on the private channel", derived(who, private))
		}
	})

	t.Run("a team's calendar: its members' time off, as the one rule lets", func(t *testing.T) {
		cal := calendarOf(sam, eventController.TeamCalendar, "teams", "teamId", team)
		if cal == "" {
			t.Fatal("Sam can't open his team's calendar")
		}
		for _, who := range []*userModels.UserInfo{sam, maya} {
			code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", who,
				`{"kind":"out_of_office","title":"Leave: a private reason","start":{"at":"`+at(2, 0)+`","zone":"UTC"},"end":{"at":"`+at(3, 0)+`"}}`, "")
			if code != http.StatusCreated {
				t.Fatalf("%s's time off: %d %v", who.UserDgraphInfo.UserName, code, body)
			}
		}
		s, m := "person:"+strings.ToLower(sam.UserDgraphInfo.Uuid), "person:"+strings.ToLower(maya.UserDgraphInfo.Uuid)
		got := derived(sam, cal)
		is(t, "Sam on the team", got, s, m)
		for k, e := range got {
			if e.Title != "Away" || e.Notes != "" || strings.Contains(e.Title, "private") {
				t.Errorf("%s's time off shows more than Away: %+v", k, e)
			}
		}
		if calendarOf(ken, eventController.TeamCalendar, "teams", "teamId", team) != "" {
			t.Error("Ken, outside the team, opens its calendar")
		}
		is(t, "Ken on the team", derived(ken, cal))

		// choice is Maya's own choice as she reads it (GET) or sets it (PUT
		// hidden), failing unless it says hidden, with the workspace on.
		choice := func(what string, rec *httptest.ResponseRecorder, hidden bool) {
			t.Helper()
			var body struct {
				Data calendarBusiness.FreeBusyChoice `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || body.Data.Hidden != hidden || !body.Data.Workspace {
				t.Errorf("%s: %d %s, want hidden %v", what, rec.Code, rec.Body.String(), hidden)
			}
		}
		mine := func() *httptest.ResponseRecorder {
			return call(t, eventController.MyFreeBusy, http.MethodGet, "/calendar/freebusy", maya, "", nil)
		}
		put := func(hidden string) *httptest.ResponseRecorder {
			return call(t, eventController.SetMyFreeBusy, http.MethodPut, "/calendar/freebusy", maya, `{"hidden":`+hidden+`}`, nil)
		}
		// Maya never chose: hers show.
		choice("Maya's choice before she makes one", mine(), false)
		// Maya keeps her busy times to herself: Sam sees his own alone.
		choice("Maya hides", put("true"), true)
		choice("Maya's choice once she hides", mine(), true)
		is(t, "Sam once Maya hides", derived(sam, cal), s)
		is(t, "Maya once she hides", derived(maya, cal), s, m)
		// She shows them again: Sam sees her Away again.
		choice("Maya shows them again", put("false"), false)
		choice("Maya's choice once she shows them again", mine(), false)
		is(t, "Sam once Maya shows them again", derived(sam, cal), s, m)
		// The workspace keeps everyone's: each sees their own alone. The
		// switch is the workspace's, read by every later test: it is put
		// back as it was when this one ends.
		by := policyBusiness.Changer{UserID: sam.UserPostgresInfo.Id, Email: sam.UserPostgresInfo.EmailID}
		was := policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFreeBusy)
		t.Cleanup(func() {
			if _, err := policyBusiness.Admin.Set(context.Background(), policyBusiness.CalendarFreeBusy.Key, json.RawMessage(strconv.FormatBool(was)), by); err != nil {
				t.Errorf("put the workspace's free/busy switch back: %v", err)
			}
		})
		if _, err := policyBusiness.Admin.Set(ctx, policyBusiness.CalendarFreeBusy.Key, json.RawMessage(`false`), by); err != nil {
			t.Fatal(err)
		}
		is(t, "Maya with the workspace off", derived(maya, cal), m)
		// Her own choice stays hers: she undid hiding, so with the workspace
		// off she still reads as not hiding, and the workspace as off.
		var off struct {
			Data calendarBusiness.FreeBusyChoice `json:"data"`
		}
		if rec := mine(); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &off) != nil || off.Data.Hidden || off.Data.Workspace {
			t.Errorf("Maya's choice with the workspace off: %d %s, want hidden false, workspace false", rec.Code, rec.Body.String())
		}
		is(t, "Sam with the workspace off", derived(sam, cal), s)
	})
}
