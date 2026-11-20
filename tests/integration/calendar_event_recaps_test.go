//go:build integration

package integration_test

// A call's recap on its event (gcal-new-event-notes), over HTTP, on the real
// stores: the recap and its action items kept on the call's node are read on
// the event by whoever may read its transcript (its organiser and the member
// it invites) and by nobody else; an action item is made a task, in a
// project its maker may add tasks to, and put on the event through the
// event's own files (591's task link), once, by whoever may change the event
// alone, never in a project they aren't in or may only read, and never from
// a deleted call; the organiser alone turns on "Send the recap to guests",
// reads it back on, and "this and following" keeps it. And migration 594
// goes up, down and up again.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	_ "github.com/akashc777/OneCamp/business/CallRoom"  // hands the calendar its event calls
	_ "github.com/akashc777/OneCamp/business/CallState" // hands business/LiveKit the event of a room's call
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnEventsCallRecapOnTheEventAndItsTasks(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	ctx := context.Background()
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")

	// Sam's project, which he and Maya may add tasks to: Maya's refusal below
	// is the event's change rule alone.
	team, project := uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Launch', $2)`, []any{team, sam.UserPostgresInfo.Id}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Launch', $2, $3)`, []any{project, team, sam.UserPostgresInfo.Id}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	samNode := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}}
	both := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}
	dg.Mutate(t, map[string]any{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": project, "project_name": "Launch",
		"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": both, "project_admins": both,
		"project_created_by": map[string]any{"uid": sam.UserDgraphInfo.Uid},
		"project_team": map[string]any{"uid": "_:team", "dgraph.type": "Team", "team_uuid": team, "team_name": "Launch",
			"team_members": both, "team_admins": samNode, "team_projects": []map[string]any{{"uid": "_:launch"}}}})

	// Two more projects, each Maya's, in a team of its own: Payroll, which Sam
	// isn't in, and Roadmap, which he is in as a viewer, so may read but not
	// add tasks to.
	payroll, roadmap := uuid.NewString(), uuid.NewString()
	mayaNode := []map[string]any{{"uid": maya.UserDgraphInfo.Uid}}
	for _, p := range []struct {
		id, name string
		members  []map[string]any
	}{{payroll, "Payroll", mayaNode}, {roadmap, "Roadmap", both}} {
		its := uuid.NewString()
		if _, err := env.PG.Exec(`INSERT INTO teams (id, team_name, created_by) VALUES ($1, $2, $3)`, its, p.name, maya.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, $2, $3, $4)`, p.id, p.name, its, maya.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": p.id, "project_name": p.name,
			"project_deleted_at": "0001-01-01T00:00:00Z", "project_members": p.members, "project_admins": mayaNode,
			"project_created_by": map[string]any{"uid": maya.UserDgraphInfo.Uid},
			"project_team": map[string]any{"uid": "_:t", "dgraph.type": "Team", "team_uuid": its, "team_name": p.name,
				"team_members": both, "team_admins": mayaNode, "team_projects": []map[string]any{{"uid": "_:p"}}}})
	}
	if _, err := env.PG.Exec(`INSERT INTO project_roles (project_id, user_id, role) VALUES ($1, $2, 'viewer')`, roadmap, sam.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}

	// Sam's event with Maya and a guest by email, its call, and the call's
	// transcript node, filed under the event.
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Launch sync","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"email":"guest@example.org","name":"Gita"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("its call: %d %v", code, body)
	}
	var room string
	if err := env.PG.QueryRowContext(ctx, `SELECT call_room FROM events WHERE id = $1`, e.ID).Scan(&room); err != nil {
		t.Fatal(err)
	}
	key := helpers.CallSessionKeyFor(uuid.NewString())
	if err := liveKitBusiness.EnsureCallSessionNode(ctx, key, room); err != nil {
		t.Fatal(err)
	}
	liveKitBusiness.EndCallSession(ctx, key, time.Now().Add(-20*time.Minute))

	// The recap writes its words and items on the call's node, notes or none.
	recap := recordingDomain.CallRecap{Text: "**📝 Summary**\n- We ship Friday.", Items: []recordingDomain.ActionItem{
		{Text: "Send the deck to legal", Owner: "Sam", Due: "2026-11-14"}, {Text: "Book the room"}}}
	if err := recordingDomain.RecordCallArtefacts(ctx, key, "", "", recap); err != nil {
		t.Fatal(err)
	}

	type recapCall struct {
		Key         string                       `json:"key"`
		Recap       string                       `json:"recap"`
		ActionItems []recordingDomain.ActionItem `json:"action_items"`
	}
	artefacts := func(who *userModels.UserInfo) (int, []recapCall) {
		code, body := v2(t, eventController.GetCalendarEventCallArtefacts, http.MethodGet, "/calendar/events/"+e.ID+"/call/artefacts", who, "", e.ID)
		var out []recapCall
		_ = json.Unmarshal(body["data"], &out)
		return code, out
	}
	for name, who := range map[string]*userModels.UserInfo{"its organiser": sam, "the member it invites": maya} {
		code, got := artefacts(who)
		if code != http.StatusOK || len(got) != 1 || got[0].Key != key || got[0].Recap != recap.Text || len(got[0].ActionItems) != 2 ||
			got[0].ActionItems[0].Owner != "Sam" || got[0].ActionItems[0].Due != "2026-11-14" {
			t.Errorf("%s reads the recap: %d %+v", name, code, got)
		}
	}
	if code, got := artefacts(lee); code != http.StatusNotFound || len(got) != 0 {
		t.Errorf("a member the event doesn't invite read its recap: %d %+v", code, got)
	}

	// Lee's own event's call, with its own recap: never read through Sam's.
	code, body = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", lee,
		`{"title":"Payroll review","start":{"at":"`+at(11)+`","zone":"Europe/London"},"end":{"at":"`+at(12)+`"}}`, "")
	if code != http.StatusCreated {
		t.Fatalf("Lee's event: %d %v", code, body)
	}
	other := dataEvent(t, body)
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+other.ID+"/call", lee, "", other.ID); code != http.StatusOK {
		t.Fatalf("Lee's call: %d %v", code, body)
	}
	var otherRoom string
	if err := env.PG.QueryRowContext(ctx, `SELECT call_room FROM events WHERE id = $1`, other.ID).Scan(&otherRoom); err != nil {
		t.Fatal(err)
	}
	otherKey := helpers.CallSessionKeyFor(uuid.NewString())
	if err := liveKitBusiness.EnsureCallSessionNode(ctx, otherKey, otherRoom); err != nil {
		t.Fatal(err)
	}
	liveKitBusiness.EndCallSession(ctx, otherKey, time.Now().Add(-10*time.Minute))
	if err := recordingDomain.RecordCallArtefacts(ctx, otherKey, "", "", recordingDomain.CallRecap{Text: "- Salaries rise 4%.",
		Items: []recordingDomain.ActionItem{{Text: "Tell finance the new salaries"}}}); err != nil {
		t.Fatal(err)
	}

	makeTaskIn := func(who *userModels.UserInfo, callKey, index, in string) (int, map[string]json.RawMessage) {
		rec := call(t, eventController.MakeCalendarEventCallActionTask, http.MethodPost,
			"/calendar/events/"+e.ID+"/call/artefacts/"+callKey+"/action-items/"+index+"/task", who, `{"project_uuid":"`+in+`"}`,
			map[string]string{"eventId": e.ID, "key": callKey, "index": index})
		var out map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	makeTaskOn := func(who *userModels.UserInfo, callKey, index string) (int, map[string]json.RawMessage) {
		return makeTaskIn(who, callKey, index, project)
	}
	makeTask := func(who *userModels.UserInfo, index string) (int, map[string]json.RawMessage) {
		return makeTaskOn(who, key, index)
	}
	tasksIn := func(in string) int {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE project_id = $1`, in).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tasks := func() int { return tasksIn(project) }
	if code, body := makeTask(maya, "0"); code != http.StatusForbidden || tasks() != 0 || !strings.Contains(string(body["msg"]), "made this event") {
		t.Errorf("Maya, who may add tasks to the project but only read the event, made a task: %d %s, %d tasks", code, body["msg"], tasks())
	}
	if code, _ := makeTask(lee, "0"); code != http.StatusNotFound || tasks() != 0 {
		t.Errorf("Lee, not on the event, made a task: %d, %d tasks", code, tasks())
	}
	if code, _ := makeTask(sam, "7"); code != http.StatusNotFound {
		t.Errorf("an item the call doesn't have: %d", code)
	}
	if code, body := makeTaskOn(sam, otherKey, "0"); code != http.StatusNotFound || tasks() != 0 || strings.Contains(fmt.Sprint(body), "finance") {
		t.Errorf("Sam read Lee's call's recap through his own event: %d %v, %d tasks", code, body, tasks())
	}
	// Sam may change the event, and the project he picks has its say too: one
	// he isn't in is no project of his (its name untold), and one he may only
	// read takes no task from him.
	if code, body := makeTaskIn(sam, key, "0", payroll); code != http.StatusForbidden || tasksIn(payroll) != 0 ||
		!strings.Contains(string(body["msg"]), "not found") || strings.Contains(string(body["msg"]), "Payroll") {
		t.Errorf("Sam, not in Payroll, made a task there: %d %s, %d tasks", code, body["msg"], tasksIn(payroll))
	}
	if code, body := makeTaskIn(sam, key, "0", roadmap); code != http.StatusForbidden || tasksIn(roadmap) != 0 ||
		!strings.Contains(string(body["msg"]), "not add tasks") {
		t.Errorf("Sam, who may only read Roadmap, made a task there: %d %s, %d tasks", code, body["msg"], tasksIn(roadmap))
	}
	code, body = makeTask(sam, "0")
	var made struct {
		TaskUUID string `json:"task_uuid"`
		Made     bool   `json:"made"`
		Linked   bool   `json:"linked"`
	}
	_ = json.Unmarshal(body["data"], &made)
	if code != http.StatusCreated || !made.Made || !made.Linked || tasks() != 1 {
		t.Fatalf("Sam makes the task: %d %v, %d tasks", code, body, tasks())
	}
	var mine int
	if err := env.PG.QueryRow(`SELECT count(*) FROM tasks WHERE id = $1 AND created_by = $2`, made.TaskUUID, sam.UserPostgresInfo.Id).Scan(&mine); err != nil || mine != 1 {
		t.Errorf("the task's row, made as Sam: %d, %v", mine, err)
	}
	var linked int
	if err := env.PG.QueryRow(`SELECT count(*) FROM calendar_event_files WHERE event_id = $1 AND ref_kind = 'task' AND ref_uuid = $2 AND deleted_at IS NULL`,
		e.ID, made.TaskUUID).Scan(&linked); err != nil || linked != 1 {
		t.Errorf("the task on the event: %d, %v; want its one link", linked, err)
	}
	if _, got := artefacts(maya); len(got) != 1 || got[0].ActionItems[0].Task != made.TaskUUID || got[0].ActionItems[1].Task != "" {
		t.Errorf("the item's task as the event shows it: %+v", got)
	}
	if code, body := makeTask(sam, "0"); code != http.StatusOK || !strings.Contains(string(body["data"]), made.TaskUUID) || tasks() != 1 {
		t.Errorf("again: %d %v, %d tasks; want the same task and no second", code, body, tasks())
	}

	// Its call deleted (softly), its items are no one's to make tasks of.
	if rec := call(t, eventController.DeleteCalendarEventCallArtefact, http.MethodDelete, "/calendar/events/"+e.ID+"/call/artefacts/"+key, sam, "",
		map[string]string{"eventId": e.ID, "key": key}); rec.Code != http.StatusOK {
		t.Fatalf("Sam deletes the call: %d %s", rec.Code, rec.Body.String())
	}
	if code, body := makeTask(sam, "1"); code != http.StatusNotFound || tasks() != 1 {
		t.Errorf("a deleted call's item made a task: %d %v, %d tasks", code, body, tasks())
	}

	// "Send the recap to guests" is Sam's alone, and off until he turns it on.
	recapToGuests := func(who *userModels.UserInfo, on string) int {
		code, _ := v2(t, eventController.SetCalendarEventRecapToGuests, http.MethodPost, "/calendar/events/"+e.ID+"/call/recap-to-guests", who, `{"on":`+on+`}`, e.ID)
		return code
	}
	stored := func() bool {
		var on bool
		if err := env.PG.QueryRow(`SELECT recap_to_guests FROM events WHERE id = $1`, e.ID).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	if stored() {
		t.Error("an event sends its recap to guests before its organiser says so")
	}
	if code := recapToGuests(maya, "true"); code != http.StatusForbidden || stored() {
		t.Errorf("Maya turned it on: %d", code)
	}
	if code := recapToGuests(sam, "true"); code != http.StatusOK || !stored() {
		t.Errorf("Sam turned it on: %d %v", code, stored())
	}
	// Sam reads it back on, and "this and following" keeps it on the new series.
	switchOf := func(id string) bool {
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+id, sam, "", id)
		if code != http.StatusOK {
			t.Fatalf("Sam reads %s: %d %v", id, code, body)
		}
		return dataEvent(t, body).RecapToGuests
	}
	if !switchOf(e.ID) {
		t.Error("Sam reads his event back with the recap to guests off")
	}
	code, body = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Weekly launch","start":{"at":"`+at(13)+`","zone":"UTC"},"end":{"at":"`+at(14)+`"},`+
			`"recurrence":{"rule":"FREQ=WEEKLY;COUNT=3"},"attendees":[{"email":"guest@example.org","name":"Gita"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("the series: %d %v", code, body)
	}
	series := dataEvent(t, body)
	if code, _ := v2(t, eventController.SetCalendarEventRecapToGuests, http.MethodPost, "/calendar/events/"+series.ID+"/call/recap-to-guests", sam, `{"on":true}`, series.ID); code != http.StatusOK {
		t.Fatalf("Sam turns it on for the series: %d", code)
	}
	var weekly []string
	var third calendarBusiness.Event
	for _, ev := range apiList(t, sam, day.Format(time.RFC3339), day.AddDate(0, 0, 22).Format(time.RFC3339)) {
		if ev.Title == "Weekly launch" {
			weekly, third = append(weekly, ev.ID), ev
		}
	}
	if len(weekly) != 3 {
		t.Fatalf("%d instances of the series", len(weekly))
	}
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+third.ID, sam,
		`{"sequence":`+itoa(third.Sequence)+`,"scope":"following","location":"Room 2"}`, third.ID)
	if code != http.StatusOK {
		t.Fatalf("this and following: %d %v", code, body)
	}
	if fresh := dataEvent(t, body); fresh.ID == series.ID || !switchOf(fresh.ID) {
		t.Errorf("the new series %s (the series %s) reads the recap to guests off", fresh.ID, series.ID)
	}
}

// A recap's notice email to the event's members shows what the recap says
// as plain text: its words are text in the HTML part, escaped once, and as
// written in the plain-text part. (The notice itself keeps them as written,
// for the Inbox and a push: business/Calendar's recap tests.)
func TestARecapsNoticeEmailShowsWhatTheRecapSaysAsPlainText(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	// Notification email on, as an install with a sending key has it; nothing
	// is sent, the queue is read.
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	// Maya is emailed whether or not she is online: no device is connected here.
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, email_only_when_offline) VALUES ($1, $2, false)
		ON CONFLICT (user_id) DO UPDATE SET email_only_when_offline = false`, maya.UserPostgresInfo.Id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Launch sync","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)

	const wrote = `We ship <a href="https://x">y</a> & <img src=z> <style>p{}</style>`
	if err := calendarBusiness.TellRecap(ctx, uuid.MustParse(e.ID), calendarBusiness.Recap{Key: "call:plain", Text: "**📝 Summary**\n- " + wrote}); err != nil {
		t.Fatal(err)
	}
	var subject, html string
	var text sql.NullString
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err := env.PG.QueryRow(`SELECT subject, html_body, text_body FROM notification_email_queue
			WHERE user_id = $1 AND event_type = $2 AND dedup_key LIKE '%' || $3 || '%'`,
			maya.UserPostgresInfo.Id, string(notificationBusiness.EventCalendarRecap), e.ID).Scan(&subject, &html, &text)
		if err == nil {
			break
		}
		if err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no recap email reached Maya")
		}
	}
	for _, raw := range []string{`<a href="https://x"`, "<img src=z>", "<style>p{}"} {
		if strings.Contains(html, raw) {
			t.Errorf("the HTML part holds %q as HTML:\n%s", raw, html)
		}
	}
	if shown := template.HTMLEscapeString(wrote); !strings.Contains(html, shown) {
		t.Errorf("the HTML part doesn't show %q, escaped once:\n%s", shown, html)
	}
	if strings.Contains(html, "&amp;amp;") || strings.Contains(html, "&amp;lt;") {
		t.Errorf("the HTML part escapes the recap twice:\n%s", html)
	}
	if !strings.Contains(text.String, wrote) {
		t.Errorf("the plain-text part doesn't show the recap as written:\n%s", text.String)
	}
	if subject == "" {
		t.Error("the recap email has no subject")
	}
}

func TestCalendarEventRecapsMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "594_calendar_event_recaps."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	// A person and an event from before 594 get recap emails, and their
	// event sends none to its guests by email.
	if _, err := env.PG.Exec(down); err != nil {
		t.Fatal(err)
	}
	before := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'before-' || $2 || '@example.test', 'before' || $2, 'Before', NOW(), NOW())`, before, before.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, before, "unsub-"+before.String()); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		sql  string
		want int
	}{{up, 2}, {down, 0}, {up, 2}, {down, 0}, {up, 2}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		var columns int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE is_nullable = 'NO' AND (
			(table_name = 'users_notification_preferences' AND column_name = 'email_calendar_recaps' AND column_default = 'true') OR
			(table_name = 'events' AND column_name = 'recap_to_guests' AND column_default = 'false'))`).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != step.want {
			t.Errorf("step %d: %d recap columns, want %d", i, columns, step.want)
		}
	}
	var recaps bool
	if err := env.PG.QueryRow(`SELECT email_calendar_recaps FROM users_notification_preferences WHERE user_id = $1`, before).Scan(&recaps); err != nil || !recaps {
		t.Errorf("a person from before 594: %v %v, want recap emails on", recaps, err)
	}
}
