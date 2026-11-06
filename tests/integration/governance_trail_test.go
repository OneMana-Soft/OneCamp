//go:build integration
// +build integration

package integration_test

// The governance trail (miro-44, gcal-new-audit-retention), over the real
// routes against Postgres 12 and Dgraph: who a board was shared with, every
// export of it, and every change to a calendar itself are rows in the
// hash-chained admin audit log, under "boards" and "calendar", and the chain
// still verifies. A board someone can't read, or a uuid naming no board,
// answers 404 and writes nothing; a private board is never named in its rows.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestEveryBoardShareAndExportIsInTheAuditLog|TestEveryCalendarChangeIsInTheAuditLog' -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	boardController "github.com/akashc777/OneCamp/controllers/Board"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	calendarModel "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// trailRow is one admin audit row about a resource.
type trailRow struct{ category, action, summary, meta, actor string }

// trailRows waits until the log holds want rows naming resource in their
// metadata (the sinks write as the bus delivers), and answers them in order.
func trailRows(t *testing.T, db *sql.DB, resource string, want int) []trailRow {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		rows, err := db.Query(`SELECT category, action, summary, metadata::text, COALESCE(actor_id::text, '') FROM admin_audit_log
			WHERE metadata::text LIKE '%' || $1 || '%' ORDER BY seq`, resource)
		if err != nil {
			t.Fatal(err)
		}
		var out []trailRow
		for rows.Next() {
			var r trailRow
			if err := rows.Scan(&r.category, &r.action, &r.summary, &r.meta, &r.actor); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		rows.Close()
		if len(out) >= want || time.Now().After(deadline) {
			if len(out) != want {
				t.Fatalf("the log holds %d rows about %s, want %d: %+v", len(out), resource, want, out)
			}
			return out
		}
	}
}

// chainVerifies says the hash chain still checks out, every row read.
func chainVerifies(t *testing.T) {
	t.Helper()
	res, err := auditBusiness.Verify(context.Background())
	if err != nil || res == nil || !res.OK {
		t.Fatalf("the audit chain doesn't verify: %+v %v", res, err)
	}
}

func TestEveryBoardShareAndExportIsInTheAuditLog(t *testing.T) {
	w := newDocsWorld(t, []string{"ada", "maya", "sam"}, "ada")
	plan := w.board(t, "maya", "Launch plan", false)
	diary := w.board(t, "maya", "Diary", true)
	gone := w.board(t, "maya", "Old ideas", false)
	if rec := w.call(boardController.DeleteBoard, "maya", `{"board_uuid":"`+gone+`"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("deleting the board: %d %s", rec.Code, rec.Body)
	}
	export := func(who, board, format string) int {
		t.Helper()
		return w.call(boardController.RecordBoardExport, who, `{"board_uuid":"`+board+`","format":"`+format+`"}`, "").Code
	}
	sam := w.people["sam"].String()

	t.Run("an export of a board someone reads is one row under boards", func(t *testing.T) {
		if code := export("sam", plan, "pdf"); code != http.StatusOK {
			t.Fatalf("Sam's export of the plan: HTTP %d", code)
		}
		r := trailRows(t, w.env.PG, plan, 1)[0]
		if r.category != "boards" || r.action != "board.exported" || r.summary != "Exported ‘Launch plan’ as a PDF, in the browser" || r.actor != sam {
			t.Fatalf("the export's row: %+v", r)
		}
	})
	t.Run("a board someone can't read, or a deleted one, answers 404 and writes nothing", func(t *testing.T) {
		if code := export("sam", diary, "picture"); code != http.StatusNotFound {
			t.Fatalf("Sam's export of Maya's private diary: HTTP %d, want 404", code)
		}
		if code := export("maya", gone, "pdf"); code != http.StatusNotFound {
			t.Fatalf("an export of a deleted board: HTTP %d, want 404", code)
		}
		if code := export("maya", plan, "gif"); code != http.StatusBadRequest {
			t.Fatalf("a format the canvas doesn't make: HTTP %d, want 400", code)
		}
		if code := export("maya", "not-a-board", "pdf"); code != http.StatusBadRequest {
			t.Fatalf("an id that isn't a uuid: HTTP %d, want 400", code)
		}
		time.Sleep(500 * time.Millisecond)
		trailRows(t, w.env.PG, diary, 0)
		trailRows(t, w.env.PG, gone, 0)
		trailRows(t, w.env.PG, plan, 1)
	})
	t.Run("a uuid that names no board answers 404 on every route that reads it, and writes nothing", func(t *testing.T) {
		// The read itself answers NotFound (models/dgraph/Board), so no route
		// answers a missing board as a failed read (a 503) or a bad request.
		none := uuid.New().String()
		if code := export("maya", none, "pdf"); code != http.StatusNotFound {
			t.Fatalf("an export of a board that isn't there: HTTP %d, want 404", code)
		}
		if rec := w.call(boardController.DeleteBoard, "maya", `{"board_uuid":"`+none+`"}`, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("deleting a board that isn't there: HTTP %d %s, want 404", rec.Code, rec.Body)
		}
		// Every other route that reads a board by its uuid (V-368): the id in
		// the path, the query or the body, as each route takes it.
		inBody := `{"board_uuid":"` + none + `","prompt":"a flow","title":"x"}`
		routes := []struct {
			name    string
			handler http.HandlerFunc
			body    string
			params  []string
			query   string
		}{
			{"GetBoardInfo", boardController.GetBoardInfo, "", []string{"board_uuid", none}, ""},
			{"GetBoardForCollab", boardController.GetBoardForCollab, "", []string{"board_uuid", none}, ""},
			{"GetBoardPermissions", boardController.GetBoardPermissions, "", nil, "?board_uuid=" + none},
			{"UpdateBoard", boardController.UpdateBoard, inBody, nil, ""},
			{"BoardCollabAuthorize", boardController.BoardCollabAuthorize, inBody, nil, ""},
			{"GenerateBoardDiagram", boardController.GenerateBoardDiagram, inBody, nil, ""},
			{"PlanBoardDiagram", boardController.PlanBoardDiagram, inBody, nil, ""},
			{"GenerateBoardDiagramStream", boardController.GenerateBoardDiagramStream, inBody, nil, ""},
			{"RefineBoardDiagram", boardController.RefineBoardDiagram, inBody, nil, ""},
			{"ClusterBoard", boardController.ClusterBoard, inBody, nil, ""},
			{"GenerateBoardUI", boardController.GenerateBoardUI, inBody, nil, ""},
			{"RefineBoardUI", boardController.RefineBoardUI, inBody, nil, ""},
		}
		for _, rt := range routes {
			var rec *httptest.ResponseRecorder
			if rt.query != "" {
				rec = w.callQuery(rt.handler, "maya", rt.query)
			} else {
				rec = w.callAt(rt.handler, "maya", false, rt.body, rt.params...)
			}
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s on a board that isn't there: HTTP %d %s, want 404", rt.name, rec.Code, rec.Body)
			}
		}
		time.Sleep(500 * time.Millisecond)
		trailRows(t, w.env.PG, none, 0)
	})
	t.Run("a link to a board made and turned off is two rows under boards with the link's id", func(t *testing.T) {
		if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
			t.Fatal(err)
		}
		rec := w.guestLink("maya", "board", plan)
		var out struct {
			Data struct {
				GrantID string `json:"grant_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Data.GrantID == "" {
			t.Fatalf("Maya makes a link to the plan: %d %s", rec.Code, rec.Body)
		}
		if rec := w.callAt(guestController.RevokeResourceGuestLink, "maya", false, "", "id", out.Data.GrantID); rec.Code != http.StatusOK {
			t.Fatalf("Maya turns it off: %d %s", rec.Code, rec.Body)
		}
		var boards []trailRow
		for _, r := range trailRows(t, w.env.PG, out.Data.GrantID, 4) {
			if r.category == "boards" {
				boards = append(boards, r)
			}
		}
		if len(boards) != 2 || boards[0].action != "board.guest_link_made" || boards[1].action != "board.guest_link_turned_off" ||
			!strings.Contains(boards[0].meta, plan) || !strings.Contains(boards[1].meta, plan) {
			t.Fatalf("the plan's own rows for its link: %+v", boards)
		}
	})
	t.Run("a share, an export and a removal on a private board never name it", func(t *testing.T) {
		if rec := w.call(boardController.UpdateBoardPermissions, "maya", `{"board_uuid":"`+diary+`","add_editors":["`+sam+`"]}`, ""); rec.Code != http.StatusOK {
			t.Fatalf("Maya shares her diary with Sam: %d %s", rec.Code, rec.Body)
		}
		trailRows(t, w.env.PG, diary, 1)
		if code := export("sam", diary, "picture"); code != http.StatusOK {
			t.Fatalf("Sam's export of the diary shared with him: HTTP %d", code)
		}
		trailRows(t, w.env.PG, diary, 2)
		if rec := w.call(boardController.UpdateBoardPermissions, "maya", `{"board_uuid":"`+diary+`","remove_editors":["`+sam+`"]}`, ""); rec.Code != http.StatusOK {
			t.Fatalf("Maya takes Sam off her diary: %d %s", rec.Code, rec.Body)
		}
		rows := trailRows(t, w.env.PG, diary, 3)
		want := []trailRow{
			{category: "boards", action: "board.shared", summary: "Shared a private board with 1 person who can edit"},
			{category: "boards", action: "board.exported", summary: "Exported a private board as a picture, in the browser"},
			// Main's ruling: a board's takings move from "docs" to "boards";
			// rows already written keep their area (the chain is never rewritten).
			{category: "boards", action: "board.access.remove", summary: "Took away or lowered people's roles on a board"},
		}
		for i, r := range rows {
			if r.category != want[i].category || r.action != want[i].action || r.summary != want[i].summary {
				t.Errorf("row %d: %+v, want %+v", i, r, want[i])
			}
			if strings.Contains(r.summary, "Diary") || strings.Contains(r.meta, "Diary") {
				t.Errorf("row %d names the private board: %+v", i, r)
			}
		}
		if !strings.Contains(rows[0].meta, sam) {
			t.Errorf("the share's row doesn't name who it reached: %s", rows[0].meta)
		}
	})
	chainVerifies(t)
}

func TestEveryCalendarChangeIsInTheAuditLog(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam, ria := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Ria")
	own, err := calendarModel.PersonalCalendar(ctx, env.PG, sam.UserPostgresInfo.Id, uuid.New(), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	riaID := ria.UserPostgresInfo.Id.String()
	share := func(level string) {
		t.Helper()
		rec := call(t, eventController.ShareCalendar, http.MethodPut, "/calendar/calendars/"+own.String()+"/shares/"+riaID, sam, `{"level":"`+level+`"}`,
			map[string]string{"calendarId": own.String(), "userId": riaID})
		if rec.Code != http.StatusNoContent {
			t.Fatalf("Sam shares his calendar with Ria at %q: HTTP %d %s", level, rec.Code, rec.Body.String())
		}
	}
	// The sinks write as the bus delivers, so each change's row is awaited
	// before the next change is made: the rows then stand in their order.
	share("details")
	trailRows(t, env.PG, own.String(), 1)
	share("details") // the same share again changes nothing
	share("edit")
	trailRows(t, env.PG, own.String(), 2)
	share("")
	trailRows(t, env.PG, own.String(), 3)
	rec := call(t, eventController.UpdateCalendar, http.MethodPatch, "/calendar/calendars/"+own.String(), sam, `{"hue":"moss"}`,
		map[string]string{"calendarId": own.String()})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Sam recolours his calendar: HTTP %d %s", rec.Code, rec.Body.String())
	}
	rows := trailRows(t, env.PG, own.String(), 4)
	want := []trailRow{
		{action: "calendar.shared", summary: "Shared a calendar with 1 person who can see details"},
		{action: "calendar.shared", summary: "Changed a calendar's share: 1 person who can edit now (could see details)"},
		{action: "calendar.unshared", summary: "Stopped sharing a calendar with 1 person (could edit)"},
		{action: "calendar.updated", summary: "Changed a calendar (colour)"},
	}
	for i, r := range rows {
		if r.category != "calendar" || r.action != want[i].action || r.summary != want[i].summary || r.actor != sam.UserPostgresInfo.Id.String() {
			t.Errorf("row %d: %+v, want %s %q by Sam under calendar", i, r, want[i].action, want[i].summary)
		}
	}
	if !strings.Contains(rows[0].meta, riaID) {
		t.Errorf("the share's row doesn't name Ria: %s", rows[0].meta)
	}
	// A link to the calendar (V-398): stopping one when none is live records
	// nothing; a link made, made anew over it, and stopped are a row each, by
	// Sam under calendar, saying what it shows and never its token or address.
	feed := func(method, body string, wantCode int) string {
		t.Helper()
		handler := eventController.MakeCalendarFeed
		if method == http.MethodDelete {
			handler = eventController.StopCalendarFeed
		}
		rec := call(t, handler, method, "/calendar/calendars/"+own.String()+"/feed", sam, body, map[string]string{"calendarId": own.String()})
		if rec.Code != wantCode {
			t.Fatalf("Sam's %s of his calendar's link: HTTP %d %s", method, rec.Code, rec.Body.String())
		}
		if method == http.MethodDelete {
			return ""
		}
		var made struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || !strings.HasSuffix(made.Data.URL, ".ics") {
			t.Fatalf("no link in %s (%v)", rec.Body.String(), err)
		}
		return made.Data.URL
	}
	feed(http.MethodDelete, "", http.StatusNoContent)
	links := []string{feed(http.MethodPut, `{"scope":"busy"}`, http.StatusOK)}
	trailRows(t, env.PG, own.String(), 5)
	links = append(links, feed(http.MethodPut, `{"scope":"full"}`, http.StatusOK))
	trailRows(t, env.PG, own.String(), 6)
	feed(http.MethodDelete, "", http.StatusNoContent)
	rows = trailRows(t, env.PG, own.String(), 7)
	for i, w := range []trailRow{
		{action: "calendar.feed.made", summary: "Made a link to a calendar (busy only)"},
		{action: "calendar.feed.made", summary: "Made a new link to a calendar (everything), stopping the old one (busy only)"},
		{action: "calendar.feed.stopped", summary: "Stopped a link to a calendar (everything)"},
	} {
		if r := rows[4+i]; r.category != "calendar" || r.action != w.action || r.summary != w.summary || r.actor != sam.UserPostgresInfo.Id.String() {
			t.Errorf("row %d: %+v, want %s %q by Sam under calendar", 4+i, r, w.action, w.summary)
		}
	}
	for _, link := range links {
		token := strings.TrimSuffix(path.Base(link), ".ics")
		for i, r := range rows {
			if strings.Contains(r.meta, token) || strings.Contains(r.summary, token) || strings.Contains(r.meta, "/public/calendar/") {
				t.Errorf("row %d tells a link or its token: %+v", i, r)
			}
		}
	}
	chainVerifies(t)
}
