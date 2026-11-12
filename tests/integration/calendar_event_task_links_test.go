//go:build integration

package integration_test

// A task linked to an event (EVLINK head A, gcal-15's recorded gap: "a doc
// or task links to an event only as a file or a notes link"), against real
// Postgres 12, Dgraph and Redis, on every surface that reads an event.
//
// The permission model (MAIN 13:4x 11 Nov): a linked doc or task shows on an
// event only to someone who may see both the event and it; everyone else
// sees nothing of it (no id, no count, no placeholder). Linking needs the
// right to change the event and the right to open the task; unlinking is
// soft, and linking again brings the same row back.
//
//   - the organiser links a task in his project; a task he can't open, and a
//     guest who may not change the event, are refused, and leave no row;
//   - the event panel's read (GET /calendar/events/{id}), /v1 get_event and
//     list_events, and MCP's get_event (AI edition) carry the task to Maya,
//     who is in its project, and nothing of it to Ken, who isn't; /entities
//     names it to Maya and answers "none" to Ken;
//   - someone who sees only that the organiser is busy, and the organiser's
//     own iCalendar export, carry none;
//   - unlinked, the row stays marked; linked again, the same row is back.
//
// A board on an event: TestAPrivateBoardOnAnEventOnEverySurface.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestAnEventsLinkedTasksOnEverySurface|TestAPrivateBoardOnAnEventOnEverySurface|TestCalendarEventTaskLinksMigrationUpDownUp)$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnEventsLinkedTasksOnEverySurface(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "evlink-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	// Sam organises; Maya and Ken are his guests, who may not change it.
	// Maya is in the launch project, Ken isn't; Bea alone is in payroll, and
	// sees Sam's calendar only as busy.
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	ken := seedCalendarPerson(t, env.PG, dg, "Ken")
	bea := seedCalendarPerson(t, env.PG, dg, "Bea")
	const live = "0001-01-01T00:00:00Z"
	launchProject, payrollProject, launch, payroll := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	user := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	dg.Mutate(t, []map[string]any{
		{"uid": "_:launchp", "dgraph.type": "Project", "project_uuid": launchProject, "project_name": "Launch", "project_deleted_at": live,
			"project_members": []map[string]any{user(sam), user(maya)}, "project_admins": []map[string]any{user(sam)}, "project_created_by": user(sam),
			"project_tasks": []map[string]any{{"uid": "_:launch"}}},
		{"uid": "_:launch", "dgraph.type": "Task", "task_uuid": launch, "task_name": "Press kit evlink", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:launchp"}, "task_created_by": user(sam)},
		{"uid": "_:payrollp", "dgraph.type": "Project", "project_uuid": payrollProject, "project_name": "Payroll", "project_deleted_at": live,
			"project_members": []map[string]any{user(bea)}, "project_admins": []map[string]any{user(bea)}, "project_created_by": user(bea),
			"project_tasks": []map[string]any{{"uid": "_:payroll"}}},
		{"uid": "_:payroll", "dgraph.type": "Task", "task_uuid": payroll, "task_name": "Salaries evlink", "task_status": "todo",
			"task_deleted_at": live, "task_project": map[string]any{"uid": "_:payrollp"}, "task_created_by": user(bea)},
	})

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Launch review","start":{"at":"`+at(9)+`","zone":"UTC"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+ken.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	path := "/calendar/events/" + e.ID + "/files"
	add := func(who *userModels.UserInfo, kind, id string) (int, map[string]json.RawMessage) {
		t.Helper()
		return v2(t, eventController.AddCalendarEventFile, http.MethodPost, path, who, `{"kind":"`+kind+`","uuid":"`+id+`"}`, e.ID)
	}
	remove := func(who *userModels.UserInfo, kind, id string) (int, map[string]json.RawMessage) {
		t.Helper()
		rec := call(t, eventController.RemoveCalendarEventFile, http.MethodDelete, path+"/"+kind+"/"+id, who, "",
			map[string]string{"eventId": e.ID, "kind": kind, "uuid": id})
		var out map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// row is the task's one row on the event: its id, and whether it is live.
	row := func(id string) (n int, rowID string, isLive bool) {
		t.Helper()
		rs, err := env.PG.Query(`SELECT id::text, deleted_at IS NULL FROM calendar_event_files WHERE event_id = $1 AND ref_kind = 'task' AND ref_uuid = $2`, e.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		for rs.Next() {
			n++
			if err := rs.Scan(&rowID, &isLive); err != nil {
				t.Fatal(err)
			}
		}
		return n, rowID, isLive
	}
	// sees is whether out carries the linked task, as want says, and never a
	// task's title nor the task nobody linked.
	sees := func(t *testing.T, surface, out string, want bool) {
		t.Helper()
		if strings.Contains(out, launch) != want {
			t.Errorf("%s: carries the task %v, want %v: %s", surface, !want, want, out)
		}
		if strings.Contains(out, "Press kit evlink") || strings.Contains(out, payroll) {
			t.Errorf("%s: carries a task's title, or the task nobody linked: %s", surface, out)
		}
	}

	t.Run("the organiser links a task he may open; nothing else is linked", func(t *testing.T) {
		if code, body := add(sam, "task", strings.ToUpper(launch)); code != http.StatusOK || fileList(filesIn(t, body)) != "task:"+launch {
			t.Fatalf("the launch task: %d %v", code, body)
		}
		if code, body := add(sam, "task", payroll); code != http.StatusForbidden || !strings.Contains(string(body["msg"]), "open") {
			t.Errorf("a task Sam can't open: %d %v", code, body)
		}
		if code, body := add(maya, "task", launch); code != http.StatusForbidden {
			t.Errorf("Maya, who may not change it: %d %v", code, body)
		}
		if code, body := add(bea, "task", payroll); code != http.StatusNotFound {
			t.Errorf("Bea, not on it: %d %v", code, body)
		}
		if n, _, _ := row(payroll); n != 0 {
			t.Error("a refused link has a row")
		}
	})

	t.Run("the event panel's read: Maya sees the task, Ken nothing of it", func(t *testing.T) {
		for _, c := range []struct {
			who  *userModels.UserInfo
			want string
		}{{sam, "task:" + launch}, {maya, "task:" + launch}, {ken, ""}} {
			code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, c.who, "", e.ID)
			if got := fileList(filesIn(t, body)); code != http.StatusOK || got != c.want {
				t.Errorf("%s reads %d %q, want %q", c.who.UserDgraphInfo.UserName, code, got, c.want)
			}
			sees(t, "the panel for "+c.who.UserDgraphInfo.UserName, string(body["data"]), c.want != "")
		}
	})

	t.Run("/entities names the task to Maya, and answers none to Ken", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"refs": []map[string]string{{"kind": "task", "id": launch}}})
		for who, want := range map[*userModels.UserInfo]string{maya: "ok", ken: "none"} {
			chips := call(t, entityLinkController.ReadEntities, http.MethodPost, "/entities", who, string(raw), nil)
			var out struct {
				Data []struct {
					Access string
					Data   map[string]any
				} `json:"data"`
			}
			if chips.Code != http.StatusOK || json.Unmarshal(chips.Body.Bytes(), &out) != nil || len(out.Data) != 1 || out.Data[0].Access != want {
				t.Errorf("%s's chip: %d %s, want %s", who.UserDgraphInfo.UserName, chips.Code, chips.Body.String(), want)
			}
		}
	})

	api := publicAPI(t, maya, ken)

	t.Run("/v1 get_event and list_events: Maya sees the task, Ken nothing of it", func(t *testing.T) {
		window := "from=" + day.Format(time.RFC3339) + "&to=" + day.Add(24*time.Hour).Format(time.RFC3339)
		for who, want := range map[*userModels.UserInfo]bool{maya: true, ken: false} {
			name := who.UserDgraphInfo.UserName
			code, out := api(who, http.MethodGet, "/v1/events/"+e.ID, nil)
			if code != http.StatusOK || !strings.Contains(out, "Launch review") {
				t.Fatalf("%s's get_event: %d %s", name, code, out)
			}
			sees(t, "/v1 get_event for "+name, out, want)
			code, out = api(who, http.MethodGet, "/v1/events?"+window, nil)
			if code != http.StatusOK || !strings.Contains(out, e.ID) {
				t.Fatalf("%s's list_events: %d %s", name, code, out)
			}
			sees(t, "/v1 list_events for "+name, out, want)
		}
	})

	t.Run("MCP get_event: Maya sees the task, Ken nothing of it", func(t *testing.T) {
		if !mcpOn(t) {
			t.Skip("MCP is the AI edition's")
		}
		for who, want := range map[*userModels.UserInfo]bool{maya: true, ken: false} {
			name := who.UserDgraphInfo.UserName
			code, out := api(who, http.MethodPost, "/v1/mcp", mcpGetEvent(e.ID))
			if code != http.StatusOK || !strings.Contains(out, "Launch review") {
				t.Fatalf("%s's MCP get_event: %d %s", name, code, out)
			}
			sees(t, "MCP get_event for "+name, out, want)
		}
	})

	t.Run("a busy view and the iCalendar export carry none", func(t *testing.T) {
		samCal, err := calendarBusiness.Events.PersonCalendar(ctx, bea, sam.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		q := "/calendar/calendars/events?ids=" + samCal.String() + "&from=" + day.Format(time.RFC3339) + "&to=" + day.Add(24*time.Hour).Format(time.RFC3339)
		req := call(t, eventController.ListEventsOnCalendars, http.MethodGet, q, bea, "", nil)
		if req.Code != http.StatusOK || !strings.Contains(req.Body.String(), "Busy") {
			t.Fatalf("Bea's look: %d %s", req.Code, req.Body.String())
		}
		sees(t, "Bea's busy view", req.Body.String(), false)
		var ics bytes.Buffer
		if _, err := calendarBusiness.Events.Export(ctx, sam, &ics); err != nil || !strings.Contains(ics.String(), "Launch review") {
			t.Fatalf("Sam's export: %v %s", err, ics.String())
		}
		sees(t, "Sam's export", ics.String(), false)
	})

	t.Run("unlinked, the row stays marked; linked again, the same row is back", func(t *testing.T) {
		_, first, _ := row(launch)
		if code, body := remove(sam, "task", launch); code != http.StatusOK || fileList(filesIn(t, body)) != "" {
			t.Fatalf("unlinked: %d %v", code, body)
		}
		if n, id, isLive := row(launch); n != 1 || id != first || isLive {
			t.Errorf("unlinked: %d rows, %s (was %s), live %v", n, id, first, isLive)
		}
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, maya, "", e.ID)
		if code != http.StatusOK || fileList(filesIn(t, body)) != "" {
			t.Errorf("Maya reads the unlinked task: %d %v", code, body)
		}
		if code, body := add(sam, "task", launch); code != http.StatusOK || fileList(filesIn(t, body)) != "task:"+launch {
			t.Fatalf("linked again: %d %v", code, body)
		}
		if n, id, isLive := row(launch); n != 1 || id != first || !isLive {
			t.Errorf("linked again: %d rows, %s (was %s), live %v", n, id, first, isLive)
		}
	})
}

// publicAPI is the public API as a client calls it: each of people's own
// calendar-read token, sent through the router (built once: it starts the
// background loops) with a JSON body when there is one.
func publicAPI(t *testing.T, people ...*userModels.UserInfo) func(who *userModels.UserInfo, method, path string, body any) (int, string) {
	t.Helper()
	tokens := make(map[*userModels.UserInfo]string, len(people))
	for _, who := range people {
		tok, err := apiTokenBusiness.CreateToken(context.Background(), "evlink", []string{apiTokenBusiness.ScopeCalendarRead}, 30, who.UserPostgresInfo.Id, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		tokens[who] = tok.Plaintext
	}
	routes := router.Routes()
	return func(who *userModels.UserInfo, method, path string, body any) (int, string) {
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+tokens[who])
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
}

// mcpOn turns MCP on for every token, with the calendar's tools on it: false
// in an edition without MCP (it is the AI edition's).
func mcpOn(t *testing.T) bool {
	t.Helper()
	if !helpers.FeatureRegistered(helpers.FeatureNameAI) {
		return false
	}
	if err := mcpBusiness.SetAdmission(context.Background(), true, "*"); err != nil {
		t.Fatalf("turn MCP on: %v", err)
	}
	for _, register := range []func() error{mcpBusiness.RegisterBridgedTools, mcpBusiness.PublishOps} {
		if err := register(); err != nil && !strings.Contains(err.Error(), "already registered") {
			t.Fatalf("register the MCP tools: %v", err)
		}
	}
	return true
}

// mcpGetEvent is MCP's get_event call for the event id names.
func mcpGetEvent(id string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_event", "arguments": map[string]any{"id": id}}}
}

// A board linked to an event (EVLINK head A; CR-B70 row 3092 T1), on every
// surface that reads an event, by the one rule (business/EntityLink
// ReadableRefs, boards by domain CanReadRefs) and the permission model (MAIN
// 13:4x 11 Nov): a board shows on an event only to someone who may see both
// the event and the board, and everyone else sees nothing of it; linking one
// needs the right to change the event and the right to open the board.
//
//   - Sam organises and lets his guests change the event. His roadmap is
//     private, shared with Ann to read, Ed to edit and Cy to comment; Ken, a
//     guest too, has no share. Bea, not on the event, keeps a private board
//     and a wall with no private flag.
//   - Sam links his roadmap, Bea's wall and his wrap-up board. Ken can't link
//     the roadmap, Sam can't link Bea's private board nor his deleted retro,
//     and a refusal leaves no row.
//   - the event panel's read, /v1 get_event and list_events, and MCP's
//     get_event (AI edition) carry the roadmap to its creator and to each
//     member it is shared with, and nothing of it to Ken (no id, no entry);
//     the wall to every reader of the event; never a board's title.
//   - the wrap-up board deleted, no surface carries it to anyone.
func TestAPrivateBoardOnAnEventOnEverySurface(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "evlink-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	ann := seedCalendarPerson(t, env.PG, dg, "Ann")
	ed := seedCalendarPerson(t, env.PG, dg, "Ed")
	cy := seedCalendarPerson(t, env.PG, dg, "Cy")
	ken := seedCalendarPerson(t, env.PG, dg, "Ken")
	bea := seedCalendarPerson(t, env.PG, dg, "Bea")
	const live = "0001-01-01T00:00:00Z"
	roadmap, wall, wrapup, beas, retro := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	user := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	people := func(us ...*userModels.UserInfo) []map[string]any {
		out := make([]map[string]any, 0, len(us))
		for _, u := range us {
			out = append(out, user(u))
		}
		return out
	}
	uids := dg.Mutate(t, []map[string]any{
		{"dgraph.type": "Board", "board_uuid": roadmap, "board_title": "Roadmap evboard", "board_private": true, "board_deleted_at": live,
			"board_created_by": user(sam), "board_reading_users": people(ann), "board_editing_users": people(ed), "board_commenting_users": people(cy)},
		{"dgraph.type": "Board", "board_uuid": wall, "board_title": "Wall evboard", "board_deleted_at": live, "board_created_by": user(bea)},
		{"uid": "_:wrapup", "dgraph.type": "Board", "board_uuid": wrapup, "board_title": "Wrap-up evboard", "board_private": false, "board_deleted_at": live,
			"board_created_by": user(sam)},
		{"dgraph.type": "Board", "board_uuid": beas, "board_title": "Bea's evboard", "board_private": true, "board_deleted_at": live,
			"board_created_by": user(bea)},
		{"dgraph.type": "Board", "board_uuid": retro, "board_title": "Retro evboard", "board_private": false,
			"board_deleted_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "board_created_by": user(sam)},
	})
	boards := []string{roadmap, wall, wrapup, beas, retro}
	named := map[string]string{roadmap: "Sam's private roadmap", wall: "Bea's wall (no private flag)", wrapup: "Sam's wrap-up board",
		beas: "Bea's private board", retro: "Sam's deleted retro"}
	titles := []string{"Roadmap evboard", "Wall evboard", "Wrap-up evboard", "Bea's evboard", "Retro evboard"}

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	guests := ""
	for _, g := range []*userModels.UserInfo{ann, ed, cy, ken} {
		guests += `,{"user_uuid":"` + g.UserDgraphInfo.Uuid + `"}`
	}
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Board review","guests_can_modify":true,"start":{"at":"`+at(9)+`","zone":"UTC"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[`+guests[1:]+`]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	add := func(who *userModels.UserInfo, id string) (int, string) {
		t.Helper()
		code, body := v2(t, eventController.AddCalendarEventFile, http.MethodPost, "/calendar/events/"+e.ID+"/files", who, `{"kind":"board","uuid":"`+id+`"}`, e.ID)
		raw, _ := json.Marshal(body)
		return code, string(raw)
	}
	rows := func(id string) (n, live int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM calendar_event_files
			WHERE event_id = $1 AND ref_kind = 'board' AND ref_uuid = $2`, e.ID, id).Scan(&n, &live); err != nil {
			t.Fatal(err)
		}
		return n, live
	}
	mcp := mcpOn(t)
	api := publicAPI(t, sam, ann, ed, cy, ken)
	window := "from=" + day.Format(time.RFC3339) + "&to=" + day.Add(24*time.Hour).Format(time.RFC3339)
	// carries checks one surface's answer: each board in sees by its id, and
	// nothing of any other: no id, no entry (every board entry, in however
	// many copies of the event the answer holds, MCP's text and its
	// structuredContent alike, names a board in sees), nor any board's title.
	carries := func(t *testing.T, surface, out string, sees []string) {
		t.Helper()
		flat := strings.NewReplacer(`\`, "", " ", "").Replace(out)
		ids := 0
		for _, id := range boards {
			want := slices.Contains(sees, id)
			if strings.Contains(out, id) != want {
				t.Errorf("%s: carries %s %v, want %v: %s", surface, named[id], !want, want, out)
			}
			if want {
				ids += strings.Count(flat, id)
			}
		}
		if entries := strings.Count(flat, `"kind":"board"`); entries != ids {
			t.Errorf("%s: %d board entries for %d ids of boards it may show: %s", surface, entries, ids, out)
		}
		for _, title := range titles {
			if strings.Contains(out, title) {
				t.Errorf("%s: names a board, %q: %s", surface, title, out)
			}
		}
	}
	// everySurface checks what each surface that reads the event carries to
	// who: the boards in sees, and nothing of the rest.
	everySurface := func(t *testing.T, who *userModels.UserInfo, sees ...string) {
		t.Helper()
		name := who.UserDgraphInfo.UserName
		want := make([]string, 0, len(sees))
		for _, id := range sees {
			want = append(want, "board:"+id)
		}
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, who, "", e.ID)
		if got := fileList(filesIn(t, body)); code != http.StatusOK || got != strings.Join(want, ",") {
			t.Errorf("the panel for %s: %d %q, want %q", name, code, got, strings.Join(want, ","))
		}
		carries(t, "the panel for "+name, string(body["data"]), sees)
		code, out := api(who, http.MethodGet, "/v1/events/"+e.ID, nil)
		if code != http.StatusOK || !strings.Contains(out, "Board review") {
			t.Fatalf("%s's get_event: %d %s", name, code, out)
		}
		carries(t, "/v1 get_event for "+name, out, sees)
		code, out = api(who, http.MethodGet, "/v1/events?"+window, nil)
		if code != http.StatusOK || !strings.Contains(out, e.ID) {
			t.Fatalf("%s's list_events: %d %s", name, code, out)
		}
		carries(t, "/v1 list_events for "+name, out, sees)
		if !mcp {
			return
		}
		code, out = api(who, http.MethodPost, "/v1/mcp", mcpGetEvent(e.ID))
		if code != http.StatusOK || !strings.Contains(out, "Board review") {
			t.Fatalf("%s's MCP get_event: %d %s", name, code, out)
		}
		carries(t, "MCP get_event for "+name, out, sees)
	}

	t.Run("Sam links his private roadmap, a board with no private flag and his wrap-up board", func(t *testing.T) {
		for _, id := range []string{roadmap, wall, wrapup} {
			if code, out := add(sam, id); code != http.StatusOK || !strings.Contains(out, id) {
				t.Errorf("Sam links %s: %d %s", named[id], code, out)
			}
			if n, live := rows(id); n != 1 || live != 1 {
				t.Errorf("%s: %d rows, %d live, want one live", named[id], n, live)
			}
		}
	})

	t.Run("nobody links a board they can't open, nor a deleted one, and a refusal leaves no row", func(t *testing.T) {
		for _, c := range []struct {
			who  *userModels.UserInfo
			id   string
			rows int
		}{{ken, roadmap, 1}, {sam, beas, 0}, {sam, retro, 0}} {
			if code, out := add(c.who, c.id); code != http.StatusForbidden || !strings.Contains(out, "you can open") {
				t.Errorf("%s links %s: %d %s, want refused", c.who.UserDgraphInfo.UserName, named[c.id], code, out)
			}
			if n, live := rows(c.id); n != c.rows || live != c.rows {
				t.Errorf("%s, refused to %s: %d rows, %d live, want %d", named[c.id], c.who.UserDgraphInfo.UserName, n, live, c.rows)
			}
		}
	})

	for _, c := range []struct {
		why  string
		who  *userModels.UserInfo
		sees []string
	}{
		{"its creator sees the private board", sam, []string{roadmap, wall, wrapup}},
		{"a member it is shared with to read sees it", ann, []string{roadmap, wall, wrapup}},
		{"a member it is shared with to edit sees it", ed, []string{roadmap, wall, wrapup}},
		{"a member it is shared with to comment sees it", cy, []string{roadmap, wall, wrapup}},
		{"a member with no share sees nothing of it, and the board with no private flag", ken, []string{wall, wrapup}},
	} {
		t.Run(c.why, func(t *testing.T) { everySurface(t, c.who, c.sees...) })
	}

	t.Run("a board deleted after it was linked shows to nobody, its row as it was", func(t *testing.T) {
		dg.Mutate(t, map[string]any{"uid": uids["wrapup"], "board_deleted_at": time.Now().UTC().Format(time.RFC3339)})
		everySurface(t, sam, roadmap, wall)
		everySurface(t, ken, wall)
		if n, live := rows(wrapup); n != 1 || live != 1 {
			t.Errorf("the deleted board's link: %d rows, %d live, want it kept", n, live)
		}
	})
}

// The task links' migration (591) goes up, down and up again over a live
// event with a doc and a task on it: down takes the live task off (marked,
// never deleted) and restores the old check, which keeps that row as it is
// (NOT VALID) and refuses a new one; up again takes tasks.
func TestCalendarEventTaskLinksMigrationUpDownUp(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	_, here, _, _ := runtime.Caller(0)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_event_task_links.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the task links' migration: %v %v", matches, err)
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read(matches[0]), read(strings.TrimSuffix(matches[0], ".up.sql")+".down.sql")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	end := start.Add(time.Hour)
	e, err := calendarBusiness.Events.Create(context.Background(), sam, calendarBusiness.EventInput{Title: "Migration review",
		Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(kind string) error {
		_, err := env.PG.Exec(`INSERT INTO calendar_event_files (id, event_id, ref_kind, ref_uuid, added_by) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), e.ID, kind, uuid.New(), sam.UserPostgresInfo.Id)
		return err
	}
	count := func(kind string) (all, live int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM calendar_event_files WHERE event_id = $1 AND ref_kind = $2`,
			e.ID, kind).Scan(&all, &live); err != nil {
			t.Fatal(err)
		}
		return all, live
	}
	for _, kind := range []string{"doc", "task"} {
		if err := insert(kind); err != nil {
			t.Fatalf("a %s, migrated: %v", kind, err)
		}
	}
	if err := insert("project"); err == nil {
		t.Error("a project was linked")
	}
	for i, step := range []string{down, up, down, up} {
		if _, err := env.PG.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		isUp := i%2 == 1
		if err := insert("task"); (err == nil) != isUp {
			t.Errorf("step %d: a new task row: %v, want allowed %v", i, err, isUp)
		}
		if err := insert("doc"); err != nil {
			t.Errorf("step %d: a doc: %v", i, err)
		}
		if all, live := count("task"); i == 0 && (all != 1 || live != 0) {
			t.Errorf("down: the task rows %d, %d live: want the one kept and marked", all, live)
		}
		if _, live := count("doc"); live != i+2 {
			t.Errorf("step %d: %d live docs, want every doc kept", i, live)
		}
	}
}
