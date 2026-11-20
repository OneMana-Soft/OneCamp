//go:build integration
// +build integration

package integration_test

// gcal-8 H1: an admin checks every Google and Outlook sync link in the
// workspace, and breaks one; never a token, an event or a provider's words,
// and never as the person.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

type syncLedgerRow struct {
	ID     string `json:"id"`
	Person struct {
		UUID string `json:"user_uuid"`
	} `json:"person"`
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	State      string `json:"state"`
	ErrorClass string `json:"error_class"`
	Refusals   int    `json:"refusals"`
	DeadSends  int    `json:"dead_sends"`
}

func TestCalendarSyncLedgerAdmin(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "sync-ledger-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("CSRF_DISABLED", "1")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	sam, maya, ada := seedCalendarPerson(t, env.PG, dg, "Sam"), seedCalendarPerson(t, env.PG, dg, "Maya"), seedCalendarPerson(t, env.PG, dg, "Ada")
	must(userBusiness.CreateAdminUser(ctx, ada.UserPostgresInfo.EmailID))

	// Each has their provider connected and a link in step: Sam with Google,
	// healthy; Maya with Outlook, two refused changes and a dead send.
	connect := func(who *userModels.UserInfo, connector, provider, account string) *models.SyncLink {
		t.Helper()
		tok, err := helpers.EncryptSecret("access-token-of-" + account)
		must(err)
		meta := `{"account_email":"` + account + `"}`
		must(integrationDomain.UpsertIntegration(ctx, "user", who.UserPostgresInfo.Id, connector, &tok, nil, nil, &meta, nil))
		link, _, err := models.StartMove(ctx, uuid.New(), who.UserPostgresInfo.Id, provider, account, time.Hour)
		must(err)
		must(models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""))
		return link
	}
	samLink := connect(sam, connectorBusiness.ProviderCalendar, "google", "sam@example.com")
	mayaLink := connect(maya, connectorBusiness.ProviderOutlookCalendar, "microsoft", "maya@example.com")
	// Outside the ledger: Maya's calendar read from an address (ics), and the
	// Google link of Dee, who has left the workspace. Gil is a guest.
	icsLink, icsErr := models.EnsureSyncLink(ctx, uuid.New(), maya.UserPostgresInfo.Id, "ics")
	must(icsErr)
	dee, gil := seedCalendarPerson(t, env.PG, dg, "Dee"), seedCalendarPerson(t, env.PG, dg, "Gil")
	deeLink := connect(dee, connectorBusiness.ProviderCalendar, "google", "dee@example.com")
	_, deeErr := env.PG.ExecContext(ctx, `UPDATE users SET deleted_at = NOW() WHERE id = $1`, dee.UserPostgresInfo.Id)
	must(deeErr)
	_, gilErr := env.PG.ExecContext(ctx, `UPDATE users SET is_external = TRUE WHERE id = $1`, gil.UserPostgresInfo.Id)
	must(gilErr)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)
	for hour, title := range []string{"Board prep", "Hiring sync"} {
		start, end := day.Add(time.Duration(9+hour)*time.Hour), day.Add(time.Duration(10+hour)*time.Hour)
		in := calendarBusiness.EventInput{Title: title, Start: calendarBusiness.When{At: &start, Zone: "UTC"}, End: calendarBusiness.When{At: &end, Zone: "UTC"}}
		if _, err := calendarBusiness.Events.Create(ctx, maya, in); err != nil {
			t.Fatalf("Maya's %q: %v", title, err)
		}
	}
	_, err := env.PG.Exec(`INSERT INTO calendar_sync_refusals (link_id, event_id, why)
		SELECT $1, id, 'googleapi: Error 400: invalid attendee raw words' FROM events WHERE created_by = $2 AND series_id IS NULL`, mayaLink.ID, maya.UserPostgresInfo.Id)
	must(err)
	// One dead calendar.sync entry of Maya's event; a done one and another
	// topic's dead one don't count.
	outbox := func(topic, state string) {
		t.Helper()
		_, err := env.PG.Exec(`INSERT INTO outbox (event_id, topic, kind, key, entities, state)
			SELECT $1, $2::varchar, 'event.changed', 'sync:' || id::text, jsonb_build_object('event', id::text), $3::varchar
			FROM events WHERE created_by = $4 AND series_id IS NULL ORDER BY start_at LIMIT 1`, uuid.New(), topic, state, maya.UserPostgresInfo.Id)
		must(err)
	}
	outbox("calendar.sync", "dead")
	outbox("calendar.sync", "done")
	outbox("search.index", "dead")

	routes := router.Routes()
	session := func(who *userModels.UserInfo) string {
		s, err := helpers.SignSessionToken(who.UserPostgresInfo.Id.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		must(err)
		return s
	}
	call := func(cookie, bearer, method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: cookie})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec
	}
	asSam, asAda, asGil := session(sam), session(ada), session(gil)
	ledger := func(query string) (map[string]syncLedgerRow, string) {
		t.Helper()
		rec := call(asAda, "", http.MethodGet, "/admin/calendar/sync"+query, "")
		var out struct {
			Data struct {
				Links []syncLedgerRow `json:"links"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil {
			t.Fatalf("Ada's ledger%s: HTTP %d %s", query, rec.Code, rec.Body.String())
		}
		byID := map[string]syncLedgerRow{}
		for _, l := range out.Data.Links {
			byID[l.ID] = l
		}
		return byID, rec.Body.String()
	}

	t.Run("an admin sees every link with its counts, never a token, an event or a provider's words", func(t *testing.T) {
		rows, body := ledger("")
		s, m := rows[samLink.ID.String()], rows[mayaLink.ID.String()]
		if s.Person.UUID != sam.UserPostgresInfo.Id.String() || s.Provider != "google" || s.Account != "sam@example.com" || s.State != "synced" ||
			s.Refusals != 0 || s.DeadSends != 0 || s.ErrorClass != "" {
			t.Errorf("Sam's row: %+v", s)
		}
		if m.Person.UUID != maya.UserPostgresInfo.Id.String() || m.Provider != "microsoft" || m.State != "synced" || m.Refusals != 2 || m.DeadSends != 1 {
			t.Errorf("Maya's row: %+v", m)
		}
		for name, id := range map[string]string{"Maya's ics link": icsLink.ID.String(), "Dee's link (Dee has left)": deeLink.ID.String()} {
			if _, ok := rows[id]; ok || strings.Contains(body, id) {
				t.Errorf("the ledger lists %s", name)
			}
		}
		for _, never := range []string{"access-token-of", "token", "Board prep", "Hiring sync", "raw words", "googleapi"} {
			if strings.Contains(body, never) {
				t.Errorf("the ledger carries %q: %s", never, body)
			}
		}
		only, _ := ledger("?provider=microsoft")
		if _, ok := only[mayaLink.ID.String()]; !ok || len(only) == 0 {
			t.Errorf("Outlook links: no Maya's: %+v", only)
		}
		for _, l := range only {
			if l.Provider != "microsoft" {
				t.Errorf("an Outlook-only page has %+v", l)
			}
		}
		if broken, _ := ledger("?state=broken"); len(broken) != 0 {
			t.Errorf("no link is broken yet: %+v", broken)
		}
		if rec := call(asAda, "", http.MethodGet, "/admin/calendar/sync?state=stale", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("a state links don't have: HTTP %d", rec.Code)
		}
		page, _ := ledger("?limit=1")
		if len(page) != 1 {
			t.Errorf("a page of one has %d", len(page))
		}
	})

	// A token acts as its member, and /admin takes no token at all (route
	// TOKEN none): a member's is refused, and so is an admin's own.
	t.Run("a member, a member's token and an admin's token are refused", func(t *testing.T) {
		token := func(who *userModels.UserInfo) string {
			t.Helper()
			tok, err := apiTokenBusiness.CreateToken(ctx, "ledger", []string{apiTokenBusiness.ScopeCalendarRead, apiTokenBusiness.ScopeCalendarWrite}, 30, who.UserPostgresInfo.Id, nil, false)
			must(err)
			return tok.Plaintext
		}
		samTok, adaTok := token(sam), token(ada)
		breakSam := `{"link_id":"` + samLink.ID.String() + `"}`
		for _, r := range []struct {
			name string
			rec  *httptest.ResponseRecorder
			code int
		}{
			{"a member's list", call(asSam, "", http.MethodGet, "/admin/calendar/sync", ""), http.StatusForbidden},
			{"a member's break", call(asSam, "", http.MethodPost, "/admin/calendar/sync/break", breakSam), http.StatusForbidden},
			// A guest holds no session here: the session gate turns them away
			// (401) before /admin's (as calendar_rooms_test.go's guest).
			{"a guest's list", call(asGil, "", http.MethodGet, "/admin/calendar/sync", ""), http.StatusUnauthorized},
			{"a guest's break", call(asGil, "", http.MethodPost, "/admin/calendar/sync/break", breakSam), http.StatusUnauthorized},
			{"a member's token's list", call("", samTok, http.MethodGet, "/admin/calendar/sync", ""), http.StatusUnauthorized},
			{"a member's token's break", call("", samTok, http.MethodPost, "/admin/calendar/sync/break", breakSam), http.StatusUnauthorized},
			{"an admin's token's list", call("", adaTok, http.MethodGet, "/admin/calendar/sync", ""), http.StatusUnauthorized},
			{"an admin's token's break", call("", adaTok, http.MethodPost, "/admin/calendar/sync/break", breakSam), http.StatusUnauthorized},
		} {
			if r.rec.Code != r.code || strings.Contains(r.rec.Body.String(), samLink.ID.String()) {
				t.Errorf("%s: HTTP %d %s", r.name, r.rec.Code, r.rec.Body.String())
			}
		}
		if l, err := models.SyncLinkOf(ctx, sam.UserPostgresInfo.Id, "google"); err != nil || l.State != models.SyncSynced {
			t.Fatalf("a refused break broke Sam's link: %+v %v", l, err)
		}
	})

	t.Run("an admin's break clears the link, is audited, and its person is told", func(t *testing.T) {
		if rec := call(asAda, "", http.MethodPost, "/admin/calendar/sync/break", `{"link_id":"`+samLink.ID.String()+`"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("Ada breaks Sam's link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		// Sent again, the break finds the link broken and its tokens gone: it
		// answers the same and isn't audited twice (the trail checks below).
		if rec := call(asAda, "", http.MethodPost, "/admin/calendar/sync/break", `{"link_id":"`+samLink.ID.String()+`"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("Ada's break sent again: HTTP %d %s", rec.Code, rec.Body.String())
		}
		l, err := models.SyncLinkOf(ctx, sam.UserPostgresInfo.Id, "google")
		if err != nil || l == nil || l.State != models.SyncBroken {
			t.Fatalf("Sam's link after the break: %+v %v", l, err)
		}
		if connectorBusiness.IsConnected(ctx, sam.UserPostgresInfo.Id, connectorBusiness.ProviderCalendar) {
			t.Errorf("Sam's Google tokens outlive the break")
		}
		if !connectorBusiness.IsConnected(ctx, maya.UserPostgresInfo.Id, connectorBusiness.ProviderOutlookCalendar) {
			t.Errorf("breaking Sam's link disconnected Maya")
		}
		rows := trailRows(t, env.PG, samLink.ID.String(), 1)
		if r := rows[0]; r.action != "calendar.sync.link.broken" || r.actor != ada.UserPostgresInfo.Id.String() ||
			!strings.Contains(r.meta, sam.UserPostgresInfo.Id.String()) || !strings.Contains(r.meta, "admin") {
			t.Errorf("the break's audit row: %+v", r)
		}
		rec := call(asSam, "", http.MethodGet, "/calendar/sync/google", "")
		var out struct {
			Data struct {
				State string `json:"state"`
				Error string `json:"error"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Data.State != models.SyncBroken || !strings.Contains(out.Data.Error, "An admin disconnected this") {
			t.Errorf("Sam isn't told an admin disconnected his link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		rows2, _ := ledger("?state=broken")
		if r := rows2[samLink.ID.String()]; r.State != models.SyncBroken || r.ErrorClass != "admin_disconnected" {
			t.Errorf("the ledger's broken page: %+v", rows2)
		}
		trailRows(t, env.PG, samLink.ID.String(), 1) // still one, now the replay has had time
	})

	t.Run("an admin's break during a first move outlives the move's end", func(t *testing.T) {
		link, _, err := models.StartMove(ctx, uuid.New(), ada.UserPostgresInfo.Id, "google", "ada@example.com", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if rec := call(asAda, "", http.MethodPost, "/admin/calendar/sync/break", `{"link_id":"`+link.ID.String()+`"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("Ada breaks her moving link: HTTP %d %s", rec.Code, rec.Body.String())
		}
		if err := models.FinishMove(ctx, link.ID, models.SyncSynced, 3, 0, nil, ""); err != nil {
			t.Fatal(err)
		}
		if l, err := models.SyncLinkOf(ctx, ada.UserPostgresInfo.Id, "google"); err != nil || l == nil || l.State != models.SyncBroken {
			t.Errorf("the move's end undid the break: %+v %v", l, err)
		}
	})

	t.Run("a break of a link no one has, or one outside the ledger, is not found", func(t *testing.T) {
		for _, body := range []string{`{"link_id":"` + uuid.New().String() + `"}`, `{"link_id":"not-an-id"}`, `{"link_id":"` + icsLink.ID.String() + `"}`} {
			rec := call(asAda, "", http.MethodPost, "/admin/calendar/sync/break", body)
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "That sync link isn't there any more.") {
				t.Errorf("break %s: HTTP %d %s", body, rec.Code, rec.Body.String())
			}
		}
	})
}
