package Integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	"github.com/akashc777/OneCamp/initializers/oauth"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// fakeGoogle is a Calendar API to point a calendar.Service at.
func fakeGoogle(t *testing.T, handler http.HandlerFunc) *calendar.Service {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	svc, err := calendar.NewService(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// fakeEventPages answers an events list with total events, perPage at a time.
func fakeEventPages(t *testing.T, total, perPage int, calls *int) *calendar.Service {
	return fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/primary/events" {
			http.NotFound(w, r)
			return
		}
		*calls++
		from, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		to := min(from+perPage, total)
		page := map[string]any{"items": []any{}}
		items := []any{}
		for i := from; i < to; i++ {
			items = append(items, map[string]any{"id": fmt.Sprintf("e%d", i), "summary": "Meeting",
				"start": map[string]string{"dateTime": "2026-10-14T10:00:00Z"}, "end": map[string]string{"dateTime": "2026-10-14T10:30:00Z"}})
		}
		page["items"] = items
		if to < total {
			page["nextPageToken"] = strconv.Itoa(to)
		}
		_ = json.NewEncoder(w).Encode(page)
	})
}

// CAL-BUG-11: every page of a busy range is read, not Google's first 250.
func TestEveryPageOfGoogleEventsIsRead(t *testing.T) {
	calls := 0
	svc := fakeEventPages(t, 600, 250, &calls)
	got, err := listGoogleEvents(context.Background(), svc, time.Now(), time.Now().AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 600 || calls != 3 {
		t.Fatalf("read %d events in %d calls, want 600 in 3", len(got), calls)
	}
	if got[599].Id != "e599" {
		t.Fatalf("the last event is %s", got[599].Id)
	}
}

func TestARangeListsAtMost2500GoogleEvents(t *testing.T) {
	calls := 0
	svc := fakeEventPages(t, 3000, 250, &calls)
	got, err := listGoogleEvents(context.Background(), svc, time.Now(), time.Now().AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxGoogleEvents || calls != 10 {
		t.Fatalf("read %d events in %d calls, want %d in 10", len(got), calls, maxGoogleEvents)
	}
}

// fakeFreeBusy serves a calendar list and FreeBusy, recording the calendars
// FreeBusy was asked about.
func fakeFreeBusy(t *testing.T, list string, answer func(ids []string) map[string]any, asked *[]string) *calendar.Service {
	var mu sync.Mutex
	return fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me/calendarList":
			if list == "" {
				http.Error(w, `{"error":{"code":403,"message":"insufficient scopes"}}`, http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, list)
		case "/freeBusy":
			var req calendar.FreeBusyRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			var ids []string
			for _, it := range req.Items {
				ids = append(ids, it.Id)
			}
			mu.Lock()
			*asked = ids
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"calendars": answer(ids)})
		default:
			http.NotFound(w, r)
		}
	})
}

const twoCalendars = `{"items":[
	{"id":"sam@example.com","primary":true,"selected":true},
	{"id":"team@group.calendar.google.com","selected":true},
	{"id":"holidays@group.v.calendar.google.com","selected":false}
]}`

// CAL-BUG-11: busy times come from every calendar the person selected in
// Google, not the primary calendar alone.
func TestGoogleBusyReadsEverySelectedCalendar(t *testing.T) {
	var asked []string
	svc := fakeFreeBusy(t, twoCalendars, func(ids []string) map[string]any {
		return map[string]any{
			"primary":                        map[string]any{"busy": []any{map[string]string{"start": "2026-10-14T09:00:00Z", "end": "2026-10-14T10:00:00Z"}}},
			"team@group.calendar.google.com": map[string]any{"busy": []any{map[string]string{"start": "2026-10-14T15:00:00Z", "end": "2026-10-14T16:00:00Z"}}},
		}
	}, &asked)
	from, to := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	spans, err := googleBusy(context.Background(), svc, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "primary,team@group.calendar.google.com" {
		t.Fatalf("FreeBusy was asked about %v, want the primary calendar and the selected team calendar", asked)
	}
	team := false
	for _, s := range spans {
		team = team || s.Start.Equal(time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC))
	}
	if len(spans) != 2 || !team {
		t.Fatalf("busy %v, want both calendars' hours, the team calendar's 15:00 included", spans)
	}
}

func TestAnUnreadableCalendarListLeavesThePrimaryCalendar(t *testing.T) {
	var asked []string
	svc := fakeFreeBusy(t, "", func([]string) map[string]any { return map[string]any{"primary": map[string]any{}} }, &asked)
	if _, err := googleBusy(context.Background(), svc, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "primary" {
		t.Fatalf("asked about %v, want the primary calendar", asked)
	}
}

// CAL-BUG-12: a calendar Google can't answer for is unread, never free.
func TestACalendarGoogleCantReadIsUnread(t *testing.T) {
	var asked []string
	svc := fakeFreeBusy(t, twoCalendars, func([]string) map[string]any {
		return map[string]any{
			"primary":                        map[string]any{"busy": []any{}},
			"team@group.calendar.google.com": map[string]any{"errors": []any{map[string]string{"domain": "global", "reason": "notFound"}}},
		}
	}, &asked)
	_, err := googleBusy(context.Background(), svc, time.Now(), time.Now().Add(time.Hour))
	if !errors.Is(err, ErrGoogleBusyUnread) {
		t.Fatalf("err %v, want ErrGoogleBusyUnread", err)
	}
}

func TestALongRangeIsAskedInParts(t *testing.T) {
	var mu sync.Mutex
	var ranges []string
	svc := fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me/calendarList":
			_, _ = io.WriteString(w, `{"items":[]}`)
		case "/freeBusy":
			var req calendar.FreeBusyRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			ranges = append(ranges, req.TimeMin+".."+req.TimeMax)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"calendars": map[string]any{"primary": map[string]any{}}})
		}
	})
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := googleBusy(context.Background(), svc, from, from.AddDate(0, 0, 45)); err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-10-01T00:00:00Z..2026-10-31T00:00:00Z", "2026-10-31T00:00:00Z..2026-11-15T00:00:00Z"}
	if strings.Join(ranges, " ") != strings.Join(want, " ") {
		t.Fatalf("asked %v, want %v", ranges, want)
	}
}

// CAL-BUG-17 and 1: the patch to Google's copy carries cleared notes and,
// when the guests are synced, an empty guest list. A patch drops empty
// values unless told to send them.
func TestGooglesCopyGetsClearedNotesAndGuests(t *testing.T) {
	var body map[string]any
	svc := fakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/calendars/primary/events/g-1" {
			http.NotFound(w, r)
			return
		}
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"id":"g-1"}`)
	})
	in := adapter.CreateOrUpdateEventInput{Title: "Review", Description: "", StartTime: "2026-10-14T10:00:00Z", EndTime: "2026-10-14T11:00:00Z"}

	if err := patchGoogleEvent(context.Background(), svc, "g-1", googleEventPatch(in, nil, true)); err != nil {
		t.Fatal(err)
	}
	if d, ok := body["description"]; !ok || d != "" {
		t.Fatalf("the patch has no empty description: %v", body)
	}
	if a, ok := body["attendees"].([]any); !ok || len(a) != 0 {
		t.Fatalf("the patch has no empty guest list: %v", body)
	}

	if err := patchGoogleEvent(context.Background(), svc, "g-1", googleEventPatch(in, nil, false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["attendees"]; ok {
		t.Fatalf("guests not synced, yet the patch sets them: %v", body)
	}

	in.Description = "Agenda"
	if err := patchGoogleEvent(context.Background(), svc, "g-1", googleEventPatch(in, []string{"maya@example.com"}, true)); err != nil {
		t.Fatal(err)
	}
	if body["description"] != "Agenda" || fmt.Sprint(body["attendees"]) != "[map[email:maya@example.com]]" {
		t.Fatalf("patch %v", body)
	}
}

// CAL-BUG-8: links made through the connectors flow hold sealed tokens, and
// links made before it hold them in the clear. Both work.
func TestStoredTokensAreReadSealedOrPlain(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "a-test-key-for-sealing")
	sealed, err := sealToken("access-token-one")
	if err != nil {
		t.Fatal(err)
	}
	if *sealed == "access-token-one" {
		t.Fatal("not sealed")
	}
	plain := "access-token-two"
	if got := storedToken(sealed); got != "access-token-one" {
		t.Errorf("sealed token read as %q", got)
	}
	if got := storedToken(&plain); got != "access-token-two" {
		t.Errorf("plain token read as %q", got)
	}
	if storedToken(nil) != "" {
		t.Error("no token reads as one")
	}
}

func TestTheLinkedAccountIsNamed(t *testing.T) {
	meta := `{"account_email":"maya@outlook.com"}`
	if got := GoogleCalendarAccount(&integrationModel.Integration{Metadata: &meta}); got != "maya@outlook.com" {
		t.Errorf("account %q", got)
	}
	empty := `{}`
	if GoogleCalendarAccount(&integrationModel.Integration{Metadata: &empty}) != "" || GoogleCalendarAccount(&integrationModel.Integration{}) != "" || GoogleCalendarAccount(nil) != "" {
		t.Error("an unknown account is named")
	}
}

// A link is its refresh token: Update and Delete tested the access token
// instead, so a link that held only a refresh token (or whose access token was
// cleared) was "not connected" for them and connected for every reader.
func TestEveryGoogleCallTakesALinkByItsRefreshToken(t *testing.T) {
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer tokens.Close()
	was := oauth.OAuthProviders.GoogleCalendarConfig
	oauth.OAuthProviders.GoogleCalendarConfig = &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: tokens.URL}}
	defer func() { oauth.OAuthProviders.GoogleCalendarConfig = was }()
	var link *integrationModel.Integration
	defer func(f func(context.Context, string, uuid.UUID, string) (*integrationModel.Integration, error)) {
		integrationOf = f
	}(integrationOf)
	integrationOf = func(context.Context, string, uuid.UUID, string) (*integrationModel.Integration, error) {
		return link, nil
	}
	me := &model.UserInfo{UserPostgresInfo: model.User{Id: uuid.New()}}
	ctx := context.Background()

	refresh := "refresh-only"
	link = &integrationModel.Integration{RefreshToken: &refresh}
	if err := UpdateGoogleCalendarEvent(ctx, "g1", adapter.CreateOrUpdateEventInput{Title: "x"}, me, false); err == nil || err.Error() == errGoogleNotLinked.Error() {
		t.Errorf("update with a refresh token only: %v, want Google asked", err)
	}
	if err := DeleteGoogleCalendarEvent(ctx, "g1", me); err == nil || err.Error() == errGoogleNotLinked.Error() {
		t.Errorf("delete with a refresh token only: %v, want Google asked", err)
	}

	link = nil
	if err := UpdateGoogleCalendarEvent(ctx, "g1", adapter.CreateOrUpdateEventInput{}, me, false); !errors.Is(err, errGoogleNotLinked) {
		t.Errorf("update unlinked: %v", err)
	}
	if err := RemoveAttendeeFromGoogleCalendarEvent(ctx, "g1", "a@b.c", me); err != nil {
		t.Errorf("remove unlinked: %v", err)
	}
	if spans, err := FetchUserGoogleBusy(ctx, uuid.New(), time.Now(), time.Now().Add(time.Hour)); spans != nil || err != nil {
		t.Errorf("busy unlinked: %v %v", spans, err)
	}
}
