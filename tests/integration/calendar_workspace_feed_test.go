//go:build integration
// +build integration

package integration_test

// basecamp-05 over the whole router: a member's link to all their calendars
// (PUT /calendar/calendars/workspace/feed, read at /public/calendar/…ics)
// and a workspace admin's list and stop (/admin/calendar/feeds), with the
// sessions, the admin gate and the workspace switch as they run.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestALinkToAllYourCalendarsOverTheRouter(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "workspace-feed-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("CSRF_DISABLED", "1")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	sam, ria, tom, ada := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Ria"), seedCalendarPerson(t, env.PG, dg, "Tom"), seedCalendarPerson(t, env.PG, dg, "Ada")
	if err := userBusiness.CreateAdminUser(ctx, ada.UserPostgresInfo.EmailID); err != nil {
		t.Fatal(err)
	}
	guest := seedCalendarPerson(t, env.PG, dg, "Gil")
	if _, err := env.PG.Exec(`UPDATE users SET is_external = TRUE WHERE id = $1`, guest.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)
	event := func(who *userModels.UserInfo, hour int, title, visibility string) {
		t.Helper()
		start, end := day.Add(time.Duration(hour)*time.Hour), day.Add(time.Duration(hour+1)*time.Hour)
		in := calendarBusiness.EventInput{Title: title, Visibility: visibility, Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}}
		if _, err := calendarBusiness.Events.Create(ctx, who, in); err != nil {
			t.Fatalf("%s's %q: %v", who.UserDgraphInfo.UserName, title, err)
		}
	}
	event(sam, 9, "Pricing review", "")
	event(sam, 11, "Doctor", "private")
	event(tom, 13, "Tom's interview loop", "")
	samCal, err := calendarBusiness.Events.PersonCalendar(ctx, sam, sam.UserPostgresInfo.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := calendarBusiness.Events.Share(ctx, sam, samCal.String(), ria.UserPostgresInfo.Id.String(), "details"); err != nil {
		t.Fatal(err)
	}

	// The wire shapes the web reads (web-contract.txt), decoded here rather
	// than through the business types, so this test runs on a tree without
	// them and fails there on what it asks, not on compiling.
	type feedPerson struct {
		UUID string `json:"user_uuid"`
	}
	type feedView struct {
		Live      bool        `json:"live"`
		URL       string      `json:"url"`
		StoppedBy *feedPerson `json:"stopped_by"`
	}
	type feedListing struct {
		ID         string     `json:"id"`
		Person     feedPerson `json:"person"`
		Target     string     `json:"target"`
		LastReadAt *time.Time `json:"last_read_at"`
	}
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
	asRia, asAda, asGuest := session(ria), session(ada), session(guest)
	makeLink := func(scope string) string {
		t.Helper()
		rec := call(asRia, http.MethodPut, "/calendar/calendars/workspace/feed", `{"scope":"`+scope+`"}`)
		var out struct {
			Data feedView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || !strings.HasSuffix(out.Data.URL, ".ics") {
			t.Fatalf("Ria's %s link to everything: HTTP %d %s", scope, rec.Code, rec.Body.String())
		}
		u, _ := url.Parse(out.Data.URL)
		return u.Path
	}
	read := func(link string) *httptest.ResponseRecorder { return call("", http.MethodGet, link, "") }
	unknown := read("/public/calendar/" + strings.Repeat("A", 43) + ".ics")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("an unknown link: HTTP %d", unknown.Code)
	}
	answersAsUnknown := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() || !reflect.DeepEqual(rec.Header(), unknown.Header()) {
			t.Errorf("%s answers HTTP %d %q %v, an unknown link HTTP %d %q %v", what, rec.Code, rec.Body.String(), rec.Header(), unknown.Code, unknown.Body.String(), unknown.Header())
		}
	}

	busy := makeLink("busy")
	if rec := read(busy); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Pricing") || !strings.Contains(rec.Body.String(), "SUMMARY:Busy") {
		t.Errorf("Ria's busy link said more than when: HTTP %d\n%s", rec.Code, rec.Body.String())
	}
	link := makeLink("full")
	answersAsUnknown("the busy link the full one replaced", read(busy))
	// Kept only as its hash, and at least 128 random bits.
	secret := strings.TrimSuffix(path.Base(link), ".ics")
	var kept string
	if err := env.PG.QueryRow(`SELECT token_hash FROM calendar_feeds WHERE person_id = $1 AND calendar_id IS NULL AND revoked_at IS NULL`, ria.UserPostgresInfo.Id).Scan(&kept); err != nil || kept != hex.EncodeToString(helpers.LinkSecretHash(secret)) || strings.Contains(kept, secret) {
		t.Errorf("Ria's link is kept as %q (%v), not as its hash", kept, err)
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) < 16 {
		t.Errorf("the link's secret is %d bytes: %v", len(b), err)
	}
	body := read(link).Body.String()
	if !strings.Contains(body, "SUMMARY:Pricing review") || !strings.Contains(body, "SUMMARY:Busy") || strings.Contains(body, "Doctor") || strings.Contains(body, "interview loop") {
		t.Errorf("Ria's link to everything shows what she may not see, or not what she may:\n%s", body)
	}

	t.Run("a calendar she stops seeing goes at the next read", func(t *testing.T) {
		if err := calendarBusiness.Events.Share(ctx, sam, samCal.String(), ria.UserPostgresInfo.Id.String(), ""); err != nil {
			t.Fatal(err)
		}
		if rec := read(link); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Pricing") {
			t.Errorf("after Sam stopped sharing: HTTP %d\n%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a guest never has one", func(t *testing.T) {
		if rec := call(asGuest, http.MethodPut, "/calendar/calendars/workspace/feed", `{"scope":"full"}`); rec.Code < 400 || strings.Contains(rec.Body.String(), "/public/calendar/") {
			t.Errorf("a guest's link to everything: HTTP %d %s", rec.Code, rec.Body.String())
		}
		// A member made a guest: her link answers as an unknown one while she
		// is one, read as she is now, never as she was when it was made.
		guestNow := func(on bool) {
			t.Helper()
			if _, err := env.PG.Exec(`UPDATE users SET is_external = $2 WHERE id = $1`, ria.UserPostgresInfo.Id, on); err != nil {
				t.Fatal(err)
			}
		}
		guestNow(true)
		answersAsUnknown("the link of a member made a guest", read(link))
		guestNow(false)
		if rec := read(link); rec.Code != http.StatusOK {
			t.Errorf("her link once she is a member again: HTTP %d", rec.Code)
		}
	})

	var listed feedListing
	t.Run("the admin list never carries a link, and only an admin of this workspace gets it", func(t *testing.T) {
		rec := call(asAda, http.MethodGet, "/admin/calendar/feeds", "")
		var out struct {
			Data struct {
				Feeds []feedListing `json:"feeds"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("Ada's list: HTTP %d %s", rec.Code, rec.Body.String())
		}
		for _, l := range out.Data.Feeds {
			if l.Target == "workspace" && l.Person.UUID == ria.UserPostgresInfo.Id.String() {
				listed = l
			}
		}
		hash := helpers.LinkSecretHash(secret)
		for _, never := range []string{secret, hex.EncodeToString(hash), base64.StdEncoding.EncodeToString(hash), "/public/calendar/", "token"} {
			if listed.ID == "" || strings.Contains(rec.Body.String(), never) {
				t.Fatalf("Ada's list carries %q, or not Ria's link: %s", never, rec.Body.String())
			}
		}
		// Ria's link was read: the list says when.
		if listed.LastReadAt == nil || time.Since(*listed.LastReadAt) > time.Hour {
			t.Errorf("Ria's read link listed as last read %v", listed.LastReadAt)
		}
		for _, who := range []struct {
			name, cookie string
			code         int
		}{{"a member", asRia, http.StatusForbidden}, {"a guest", asGuest, http.StatusForbidden}} {
			for _, r := range []*httptest.ResponseRecorder{call(who.cookie, http.MethodGet, "/admin/calendar/feeds", ""), call(who.cookie, http.MethodDelete, "/admin/calendar/feeds/"+listed.ID, "")} {
				if r.Code != who.code && !(who.name == "a guest" && r.Code == http.StatusUnauthorized) || strings.Contains(r.Body.String(), listed.ID) {
					t.Errorf("%s at the admin list or stop: HTTP %d %s", who.name, r.Code, r.Body.String())
				}
			}
		}
		// Another workspace's admin: a session its own server signed.
		t.Setenv("JWT_SECRET", "another-workspace-secret")
		foreign := session(ada)
		t.Setenv("JWT_SECRET", "workspace-feed-integration-secret")
		for _, r := range []*httptest.ResponseRecorder{call(foreign, http.MethodGet, "/admin/calendar/feeds", ""), call(foreign, http.MethodDelete, "/admin/calendar/feeds/"+listed.ID, "")} {
			if r.Code != http.StatusUnauthorized || strings.Contains(r.Body.String(), listed.ID) {
				t.Errorf("another workspace's admin: HTTP %d %s", r.Code, r.Body.String())
			}
		}
		if rec := read(link); rec.Code != http.StatusOK {
			t.Errorf("a refused stop stopped the link: HTTP %d", rec.Code)
		}
	})

	t.Run("however often an app asks, a link is marked read at most once an hour", func(t *testing.T) {
		state := func() (string, string) {
			t.Helper()
			var xmin, at string
			if err := env.PG.QueryRow(`SELECT xmin::text, COALESCE(last_read_at::text, '') FROM calendar_feeds WHERE id = $1`, listed.ID).Scan(&xmin, &at); err != nil {
				t.Fatal(err)
			}
			return xmin, at
		}
		x1, at1 := state()
		if at1 == "" {
			t.Fatal("a read link has no last_read_at")
		}
		for i := 0; i < 5; i++ {
			if rec := read(link); rec.Code != http.StatusOK {
				t.Fatalf("read %d: HTTP %d", i, rec.Code)
			}
		}
		if x2, at2 := state(); x2 != x1 || at2 != at1 {
			t.Errorf("5 more reads within the hour rewrote the row: xmin %s -> %s, last_read_at %s -> %s", x1, x2, at1, at2)
		}
		if _, err := env.PG.Exec(`UPDATE calendar_feeds SET last_read_at = NOW() - interval '61 minutes' WHERE id = $1`, listed.ID); err != nil {
			t.Fatal(err)
		}
		_, old := state()
		if rec := read(link); rec.Code != http.StatusOK {
			t.Fatalf("read after the hour: HTTP %d", rec.Code)
		}
		if _, at3 := state(); at3 == old {
			t.Errorf("a read after the hour did not mark the link read (%s)", at3)
		}
	})

	t.Run("an admin's stop kills the link, is audited, and its person sees who stopped it", func(t *testing.T) {
		if rec := call(asAda, http.MethodDelete, "/admin/calendar/feeds/"+listed.ID, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("Ada stops Ria's link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		answersAsUnknown("a link an admin stopped", read(link))
		rows := trailRows(t, env.PG, listed.ID, 2)
		if r := rows[1]; r.action != "calendar.feed.stopped" || r.summary != "Stopped a member's link to all their calendars (everything)" || r.actor != ada.UserPostgresInfo.Id.String() || !strings.Contains(r.meta, ria.UserPostgresInfo.Id.String()) {
			t.Errorf("the stop's audit row: %+v", r)
		}
		rec := call(asRia, http.MethodGet, "/calendar/calendars/workspace/feed", "")
		var out struct {
			Data feedView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Data.Live || out.Data.StoppedBy == nil || out.Data.StoppedBy.UUID != ada.UserPostgresInfo.Id.String() {
			t.Errorf("Ria isn't shown who stopped her link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		// A second stop of the stopped link answers 404 and changes nothing: the
		// row is not rewritten (who stopped it, when) and the trail gains no row.
		stopped := func() string {
			t.Helper()
			var s string
			if err := env.PG.QueryRow(`SELECT xmin::text || ' ' || COALESCE(revoked_by::text, '') || ' ' || COALESCE(revoked_at::text, '') FROM calendar_feeds WHERE id = $1`, listed.ID).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		before := stopped()
		if rec := call(asAda, http.MethodDelete, "/admin/calendar/feeds/"+listed.ID, ""); rec.Code != http.StatusNotFound {
			t.Errorf("Ada stops the stopped link again: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if after := stopped(); after != before {
			t.Errorf("a second stop rewrote the link: %s, was %s", after, before)
		}
		for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(200 * time.Millisecond) {
			trailRows(t, env.PG, listed.ID, 2)
		}
	})

	t.Run("the workspace switch off stops every link", func(t *testing.T) {
		again := makeLink("full")
		if rec := call(asAda, http.MethodPut, "/admin/policies/calendar.feeds.allowed", `{"value":false}`); rec.Code != http.StatusOK {
			t.Fatalf("turning links off: HTTP %d %s", rec.Code, rec.Body.String())
		}
		answersAsUnknown("a link with links turned off", read(again))
		if rec := call(asAda, http.MethodPut, "/admin/policies/calendar.feeds.allowed", `{"value":true}`); rec.Code != http.StatusOK {
			t.Fatalf("turning links on: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if rec := read(again); rec.Code != http.StatusOK {
			t.Errorf("the link once links are on again: HTTP %d", rec.Code)
		}
	})

	// basecamp-05 H1: her link dialog knows links are off before she makes
	// one; the read says so, the same for every member, with no admin data.
	t.Run("a member's link read says whether the workspace allows links", func(t *testing.T) {
		for _, on := range []string{"false", "true"} {
			if rec := call(asAda, http.MethodPut, "/admin/policies/calendar.feeds.allowed", `{"value":`+on+`}`); rec.Code != http.StatusOK {
				t.Fatalf("turning links %s: HTTP %d %s", on, rec.Code, rec.Body.String())
			}
			rec := call(asRia, http.MethodGet, "/calendar/calendars/workspace/feed", "")
			var out struct {
				Data struct {
					LinksAllowed *bool `json:"links_allowed"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Data.LinksAllowed == nil || fmt.Sprint(*out.Data.LinksAllowed) != on {
				t.Errorf("with links allowed %s, Ria's read: HTTP %d %s", on, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("its person stops their own link, and is told no one else did", func(t *testing.T) {
		mine := makeLink("full")
		var mineID string
		if err := env.PG.QueryRow(`SELECT id::text FROM calendar_feeds WHERE person_id = $1 AND calendar_id IS NULL AND revoked_at IS NULL`, ria.UserPostgresInfo.Id).Scan(&mineID); err != nil {
			t.Fatal(err)
		}
		if rec := call(asRia, http.MethodDelete, "/calendar/calendars/workspace/feed", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("Ria stops her link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		answersAsUnknown("a link its person stopped", read(mine))
		// Its trail is under the link's id, as an admin's stop is: made, then
		// stopped by Ria herself, never under the nil uuid.
		rows := trailRows(t, env.PG, mineID, 2)
		if rows[0].action != "calendar.feed.made" || rows[1].action != "calendar.feed.stopped" || rows[1].actor != ria.UserPostgresInfo.Id.String() || strings.Contains(rows[1].meta, `"by"`) {
			t.Errorf("Ria's own stop's audit rows: %+v", rows)
		}
		rec := call(asRia, http.MethodGet, "/calendar/calendars/workspace/feed", "")
		var out struct {
			Data feedView `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Data.Live || out.Data.StoppedBy != nil {
			t.Errorf("Ria's own stop: HTTP %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a person deactivated loses their link at the next read", func(t *testing.T) {
		last := makeLink("full")
		if err := userBusiness.DeactivateUser(ctx, ria.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		answersAsUnknown("the link of a deactivated person", read(last))
	})
}

// Migration 600 goes up, down and up again on Postgres 12, each way twice:
// down stops every link to everything (a stop, never a delete) and takes
// away who stopped a link and when one was last read; up brings them back.
func TestCalendarFeedTargetsMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	file := func(dir string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "600_calendar_feed_targets."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := file("up"), file("down")
	env, dg := calendarStores(t)
	ctx := context.Background()
	ria := seedCalendarPerson(t, env.PG, dg, "Ria")
	_, hash, err := helpers.NewLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	feed := uuid.New()
	if err := models.InsertFeed(ctx, models.Feed{ID: feed, PersonID: ria.UserPostgresInfo.Id, TokenHash: hash, Scope: models.FeedFull}); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {down, false}, {up, true}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, column := range []string{"revoked_by", "last_read_at"} {
			if got := columnExists(t, env, "calendar_feeds", column); got != step.want {
				t.Fatalf("step %d: calendar_feeds.%s exists = %v", i, column, got)
			}
		}
		for _, index := range []string{"calendar_feeds_workspace_live_idx", "calendar_feeds_live_made_idx"} {
			if got := tableExists(t, env.PG, index); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, index, got)
			}
		}
		// revoked_by is a foreign key, so an index leads with it
		// (TestEveryForeignKeyHasAnIndex): there after up, gone after down.
		var byIndex int
		if err := env.PG.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'calendar_feeds' AND indexname = 'calendar_feeds_revoked_by_idx' AND indexdef LIKE '%USING btree (revoked_by)%'`).Scan(&byIndex); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 1, false: 0}[step.want]; byIndex != want {
			t.Fatalf("step %d: %d indexes lead with calendar_feeds.revoked_by, want %d", i, byIndex, want)
		}
		var rows, live int
		if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL) FROM calendar_feeds WHERE id = $1`, feed).Scan(&rows, &live); err != nil {
			t.Fatal(err)
		}
		if wantLive := map[bool]int{true: 1, false: 0}[i == 0]; rows != 1 || live != wantLive {
			t.Fatalf("step %d: the link to everything is %d rows, %d live", i, rows, live)
		}
	}
}
