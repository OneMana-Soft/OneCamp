//go:build integration
// +build integration

package integration_test

// V-594, through the real router: a calendar moved from Google that is
// see-only there says so in the calendar list (source "google", read_only),
// and a new event on it is refused up front with 409 calendar_read_only,
// before anything is written, where it used to be made and then refused by
// Google at the push with nobody told. A calendar Google lets OneCamp write to
// takes the event, and a PATCH naming the see-only calendar leaves the event
// where it was (an event never moves between calendars). Google's own events
// still come into the see-only calendar: a pull never asks the refusal, and
// nothing is ever sent to it.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASeeOnlyGoogleCalendarIsSaidAndRefusedUpFront -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	calendarModel "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// holidaysAtGoogle is Google for a person with a see-only calendar
// (holidays@group) that has a new holiday, and a team's calendar
// (team@group) they may write to; it keeps what OneCamp sent it.
type holidaysAtGoogle struct {
	mu     sync.Mutex
	pushed []string
}

func (*holidaysAtGoogle) Provider() string { return "google" }

func (*holidaysAtGoogle) Calendars(context.Context, calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	return nil, nil
}

func (*holidaysAtGoogle) Pull(_ context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	if cal.ID != "holidays@group" {
		return nil, cursor, nil
	}
	return []calendarSyncBusiness.Change{{ExternalID: "diwali", ETag: "e1",
		Event: ICS.Event{UID: "diwali@google.com", Summary: "Diwali", StartDate: "2026-11-08", EndDate: "2026-11-09"}}}, "c-hol-2", nil
}

func (g *holidaysAtGoogle) Push(_ context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pushed = append(g.pushed, op.Kind+" "+op.Event.Summary+" to "+op.Remote.ID)
	return calendarSyncBusiness.Ref{ExternalID: "g-" + op.EventUUID.String(), ETag: "e1"}, nil
}

func (*holidaysAtGoogle) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, nil
}

func (*holidaysAtGoogle) Stop(context.Context, calendarSyncBusiness.Watch) error { return nil }

func TestASeeOnlyGoogleCalendarIsSaidAndRefusedUpFront(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "calendar-read-only-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	// Who may call is what the member gate decides; CSRF has its own tests.
	t.Setenv("CSRF_DISABLED", "1")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupDgraph(t)
	db := postgresInit.DBConn.SqlDB
	integration.MarkCalendarMoved(t, db)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	sam, _, err := userBusiness.CreateUserWithMethod(ctx, "sam@read-only.test", "Sam", nil, userModels.AuthMethodEmail, false)
	must(err)
	// Sam's move from Google: a see-only calendar of holidays and a team's
	// calendar he may write to, each a calendar of its name, kept in step.
	link, err := calendarModel.EnsureSyncLink(ctx, uuid.New(), sam, "google")
	must(err)
	seeOnly, err := calendarModel.ImportedCalendar(ctx, sam, "Holidays in India", uuid.New(), "UTC")
	must(err)
	writable, err := calendarModel.ImportedCalendar(ctx, sam, "Team offsites", uuid.New(), "UTC")
	must(err)
	must(calendarModel.KeepSyncCalendar(ctx, calendarModel.SyncCalendar{LinkID: link.ID, RemoteID: "holidays@group", Name: "Holidays in India", CalendarID: &seeOnly, Cursor: "c-hol", ReadOnly: true}))
	must(calendarModel.KeepSyncCalendar(ctx, calendarModel.SyncCalendar{LinkID: link.ID, RemoteID: "team@group", Name: "Team offsites", CalendarID: &writable, Cursor: "c-team"}))

	routes := router.Routes()
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		token, err := helpers.SignSessionToken(sam.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		must(err)
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	eventsOn := func(cal uuid.UUID) int {
		t.Helper()
		var n int
		must(db.QueryRow(`SELECT count(*) FROM events WHERE calendar_id = $1 AND deleted_at IS NULL`, cal).Scan(&n))
		return n
	}

	// The list says where each calendar comes from, and which is read-only.
	code, out := call(http.MethodGet, "/calendar/calendars", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /calendar/calendars: %d %v", code, out)
	}
	listed := map[string]map[string]any{}
	data, _ := out["data"].([]any)
	for _, d := range data {
		v, _ := d.(map[string]any)
		id, _ := v["id"].(string)
		listed[id] = v
	}
	for _, c := range []struct {
		name     string
		cal      uuid.UUID
		readOnly any
	}{{"the see-only calendar", seeOnly, true}, {"the writable calendar", writable, nil}} {
		v := listed[c.cal.String()]
		if v == nil || v["source"] != "google" || v["read_only"] != c.readOnly {
			t.Errorf("%s is listed as %v, want source google and read_only %v", c.name, v, c.readOnly)
		}
	}
	for id, v := range listed {
		if v["kind"] == "personal" && (v["source"] != "onecamp" || v["read_only"] != nil) {
			t.Errorf("Sam's own calendar %s is listed as %v, want source onecamp and not read-only", id, v)
		}
	}

	// A new event on the see-only calendar is refused up front, in words.
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	lunch := func(cal uuid.UUID) map[string]any {
		return map[string]any{"title": "Lunch", "calendar_id": cal.String(),
			"start": map[string]any{"at": start.Format(time.RFC3339), "zone": "UTC"},
			"end":   map[string]any{"at": start.Add(time.Hour).Format(time.RFC3339), "zone": "UTC"}}
	}
	code, out = call(http.MethodPost, "/calendar/events", lunch(seeOnly))
	if code != http.StatusConflict || out["code"] != "calendar_read_only" || out["msg"] != "This calendar is read-only in Google. Put the event on another calendar." {
		t.Fatalf("a new event on the see-only calendar: %d %v, want 409 calendar_read_only", code, out)
	}
	if n := eventsOn(seeOnly); n != 0 {
		t.Fatalf("the refused event was written: %d events on the see-only calendar", n)
	}

	// On the calendar Google lets OneCamp write to, it is made.
	code, out = call(http.MethodPost, "/calendar/events", lunch(writable))
	made, _ := out["data"].(map[string]any)
	id, _ := made["id"].(string)
	if code != http.StatusCreated || id == "" || made["calendar_id"] != writable.String() {
		t.Fatalf("a new event on the writable calendar: %d %v, want 201 on it", code, out)
	}

	// A change that names the see-only calendar never moves the event there.
	code, out = call(http.MethodPatch, "/calendar/events/"+id, map[string]any{"sequence": made["sequence"], "title": "Team lunch", "calendar_id": seeOnly.String()})
	if code != http.StatusOK {
		t.Fatalf("PATCH naming the see-only calendar: %d %v", code, out)
	}
	code, out = call(http.MethodGet, "/calendar/events/"+id, nil)
	got, _ := out["data"].(map[string]any)
	if code != http.StatusOK || got["calendar_id"] != writable.String() || got["title"] != "Team lunch" {
		t.Fatalf("after the PATCH: %d %v, want Team lunch still on the writable calendar", code, out)
	}
	if n := eventsOn(seeOnly); n != 0 {
		t.Fatalf("%d events on the see-only calendar after the PATCH, want 0", n)
	}

	// Google's own events still come into the see-only calendar, and only
	// the writable calendar's event is sent to Google.
	g := &holidaysAtGoogle{}
	must(calendarBusiness.SyncLinkForTest(ctx, link, g))
	var diwali int
	must(db.QueryRow(`SELECT count(*) FROM events WHERE calendar_id = $1 AND title = 'Diwali' AND deleted_at IS NULL`, seeOnly).Scan(&diwali))
	if diwali != 1 {
		t.Fatalf("Google's holiday on the see-only calendar: %d, want 1", diwali)
	}
	if want := []string{"create Team lunch to team@group"}; !slices.Equal(g.pushed, want) {
		t.Fatalf("sent to Google: %q, want %q", g.pushed, want)
	}
}
