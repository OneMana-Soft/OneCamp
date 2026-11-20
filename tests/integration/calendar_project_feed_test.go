//go:build integration
// +build integration

package integration_test

// A link to a project's calendar (basecamp-05 head 3): made over the router
// the way the web makes one (POST /calendar/projects/{id}/calendar, then PUT
// /calendar/calendars/{id}/feed), read at /public/calendar/….ics by another
// app with no session. The link has no request to name its person, so the
// feed reads the project as its person by uuid alone; the project's name and
// standing are read by the person's graph uid.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestALinkToAProjectCalendarOverTheRouter(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "project-feed-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("CSRF_DISABLED", "1")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	ria, ben, out := seedCalendarPerson(t, env.PG, dg, "Ria"), seedCalendarPerson(t, env.PG, dg, "Ben"), seedCalendarPerson(t, env.PG, dg, "Oli")

	const live = "0001-01-01T00:00:00Z"
	person := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	launch := uuid.NewString()
	uids := dg.Mutate(t, []map[string]any{{
		"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch, "project_name": "Q4 launch h3", "project_deleted_at": live,
		"project_members": []map[string]any{person(ria), person(ben)}, "project_admins": []map[string]any{person(ben)}, "project_created_by": person(ben),
	}})

	routes := router.Routes()
	session := func(who *userModels.UserInfo) string {
		s, err := helpers.SignSessionToken(who.UserPostgresInfo.Id.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	call := func(cookie, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: cookie})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}
	asRia, asOut := session(ria), session(out)

	// The project's calendar, as the web opens it, with an event on it.
	rec := call(asRia, http.MethodPost, "/calendar/projects/"+launch+"/calendar", "")
	var opened struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); rec.Code != http.StatusOK || err != nil || opened.Data.ID == "" {
		t.Fatalf("Ria opens the project's calendar: HTTP %d %s", rec.Code, rec.Body.String())
	}
	cal := opened.Data.ID
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2).Add(10 * time.Hour)
	end := start.Add(time.Hour)
	in := calendarBusiness.EventInput{Title: "Launch review h3", CalendarID: cal, Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}}
	if _, err := calendarBusiness.Events.Create(ctx, ben, in); err != nil {
		t.Fatalf("Ben puts the review on the project's calendar: %v", err)
	}

	// Someone outside the project gets no link to it.
	if rec := call(asOut, http.MethodPut, "/calendar/calendars/"+cal+"/feed", `{"scope":"full"}`); rec.Code == http.StatusOK {
		t.Errorf("Oli, outside the project, made a link to its calendar: HTTP %d %s", rec.Code, rec.Body.String())
	}

	rec = call(asRia, http.MethodPut, "/calendar/calendars/"+cal+"/feed", `{"scope":"full"}`)
	var made struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &made); rec.Code != http.StatusOK || err != nil || !strings.HasSuffix(made.Data.URL, ".ics") {
		t.Fatalf("Ria makes a link to the project's calendar: HTTP %d %s", rec.Code, rec.Body.String())
	}
	u, _ := url.Parse(made.Data.URL)
	link := u.Path
	read := func() *httptest.ResponseRecorder { return call("", http.MethodGet, link, "") }

	got := read()
	body := got.Body.String()
	if got.Code != http.StatusOK || !strings.HasPrefix(got.Header().Get("Content-Type"), "text/calendar") || !strings.HasPrefix(body, "BEGIN:VCALENDAR") {
		t.Fatalf("another app reads Ria's link to the project's calendar: HTTP %d %q %.300s", got.Code, got.Header().Get("Content-Type"), body)
	}
	if !strings.Contains(body, "SUMMARY:Launch review h3") {
		t.Errorf("the project's link leaves out the review on its calendar:\n%.600s", body)
	}
	if !strings.Contains(body, "Q4 launch h3") {
		t.Errorf("the project's link isn't named for the project:\n%.400s", body)
	}

	// Ria leaves the project: her link to its calendar answers as one that
	// isn't there, and comes back with her (the link was never stopped).
	drop, _ := json.Marshal(map[string]any{"uid": uids["launch"], "project_members": []map[string]any{person(ria)}})
	if _, err := dgraphInit.DoCommitNow(ctx, &api.Request{Mutations: []*api.Mutation{{DeleteJson: drop}}}); err != nil {
		t.Fatalf("taking Ria out of the project: %v", err)
	}
	if rec := read(); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Launch review") {
		t.Errorf("Ria's link after she left the project: HTTP %d %.200s", rec.Code, rec.Body.String())
	}
	dg.Mutate(t, map[string]any{"uid": uids["launch"], "project_members": []map[string]any{person(ria)}})
	if rec := read(); rec.Code != http.StatusOK {
		t.Errorf("Ria's link once she is back in the project: HTTP %d %.200s", rec.Code, rec.Body.String())
	}

	// Stopped, it answers as one that isn't there.
	if rec := call(asRia, http.MethodDelete, "/calendar/calendars/"+cal+"/feed", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("Ria stops her link: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if rec := read(); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Launch review") {
		t.Errorf("Ria's stopped link: HTTP %d %.200s", rec.Code, rec.Body.String())
	}
}

// A link to a project's calendar carries the project's dated tasks, as its
// person may read each now (basecamp-05, main 16 Nov 11:5x): a project with
// dated tasks and no events serves them, each all day on its date under one
// UID that stays the same read after read; a restricted task its person may
// not read is not in it; and a stopped link still serves nothing.
func TestALinkToAProjectCalendarCarriesItsDatedTasks(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "project-feed-tasks-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("CSRF_DISABLED", "1")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ria, ben := seedCalendarPerson(t, env.PG, dg, "Ria"), seedCalendarPerson(t, env.PG, dg, "Ben")

	const live = "0001-01-01T00:00:00Z"
	person := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	launch, plan, vault := uuid.NewString(), uuid.NewString(), uuid.NewString()
	due := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 3)
	dg.Mutate(t, []map[string]any{
		{"uid": "_:launch", "dgraph.type": "Project", "project_uuid": launch, "project_name": "Q4 launch pfeed", "project_deleted_at": live,
			"project_members": []map[string]any{person(ria), person(ben)}, "project_admins": []map[string]any{person(ben)}, "project_created_by": person(ben),
			"project_tasks": []map[string]any{{"uid": "_:plan"}, {"uid": "_:vault"}}},
		{"uid": "_:plan", "dgraph.type": "Task", "task_uuid": plan, "task_name": "Launch plan pfeed", "task_status": "todo", "task_deleted_at": live,
			"task_due_date": due.Format(time.RFC3339), "task_project": map[string]any{"uid": "_:launch"}, "task_created_by": person(ben)},
		// Restricted below, made by Ben and on no one: Ria may not read it.
		{"uid": "_:vault", "dgraph.type": "Task", "task_uuid": vault, "task_name": "Pricing vault pfeed", "task_status": "todo", "task_deleted_at": live,
			"task_due_date": due.AddDate(0, 0, 1).Format(time.RFC3339), "task_project": map[string]any{"uid": "_:launch"},
			"task_created_by": person(ben)},
	})
	// Ben restricts the vault the way the app does (its row in Postgres, on the
	// project's and the task's rows there).
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id, project_name, created_by) VALUES ($1, 'Q4 launch pfeed', $2)`, []any{launch, ben.UserPostgresInfo.Id}},
		{`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $3, $4), ($2, $3, $4)`, []any{plan, vault, launch, ben.UserPostgresInfo.Id}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("seeding the project's rows: %v", err)
		}
	}
	if err := restrictionModel.Restrict(context.Background(), uuid.MustParse(vault), uuid.MustParse(launch), ben.UserPostgresInfo.Id, nil, nil, time.Now()); err != nil {
		t.Fatalf("Ben restricts the vault: %v", err)
	}

	routes := router.Routes()
	s, err := helpers.SignSessionToken(ria.UserPostgresInfo.Id.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	call := func(cookie, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: cookie})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}
	var opened, made struct {
		Data struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"data"`
	}
	rec := call(s, http.MethodPost, "/calendar/projects/"+launch+"/calendar", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); rec.Code != http.StatusOK || err != nil || opened.Data.ID == "" {
		t.Fatalf("Ria opens the project's calendar: HTTP %d %.300s", rec.Code, rec.Body.String())
	}
	cal := opened.Data.ID
	rec = call(s, http.MethodPut, "/calendar/calendars/"+cal+"/feed", `{"scope":"full"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &made); rec.Code != http.StatusOK || err != nil || !strings.HasSuffix(made.Data.URL, ".ics") {
		t.Fatalf("Ria makes a link to the project's calendar: HTTP %d %.300s", rec.Code, rec.Body.String())
	}
	u, _ := url.Parse(made.Data.URL)
	link := u.Path

	got := call("", http.MethodGet, link, "")
	body := got.Body.String()
	if got.Code != http.StatusOK || !strings.HasPrefix(body, "BEGIN:VCALENDAR") {
		t.Fatalf("another app reads Ria's link to the project's calendar: HTTP %d %.300s", got.Code, body)
	}
	if n := strings.Count(body, "BEGIN:VEVENT"); n != 1 {
		t.Errorf("the project's link carries %d entries, want 1 (its one task Ria may read):\n%.900s", n, body)
	}
	day := due.Format("20060102")
	for _, want := range []string{"SUMMARY:Launch plan pfeed", "DTSTART;VALUE=DATE:" + day, "DTEND;VALUE=DATE:" + due.AddDate(0, 0, 1).Format("20060102"), "UID:task-" + plan + "@onecamp"} {
		if !strings.Contains(body, want) {
			t.Errorf("the project's link: want %q in\n%.900s", want, body)
		}
	}
	if strings.Contains(body, "Pricing vault") || strings.Contains(body, vault) {
		t.Errorf("the project's link carries a restricted task Ria may not read:\n%.900s", body)
	}
	if again := call("", http.MethodGet, link, "").Body.String(); !strings.Contains(again, "UID:task-"+plan+"@onecamp") {
		t.Errorf("the task's UID changed between reads:\n%.600s", again)
	}

	// Stopped, it answers as one that isn't there.
	if rec := call(s, http.MethodDelete, "/calendar/calendars/"+cal+"/feed", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("Ria stops her link: HTTP %d %.200s", rec.Code, rec.Body.String())
	}
	if rec := call("", http.MethodGet, link, ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "Launch plan") {
		t.Errorf("Ria's stopped link: HTTP %d %.200s", rec.Code, rec.Body.String())
	}
}
