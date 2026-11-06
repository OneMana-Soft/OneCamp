//go:build integration
// +build integration

package integration_test

// Retention rules and holds (the governance trail's H2, gcal-new-audit-
// retention), over the real routes against Postgres 12: only admins reach
// them; a rule removes calendar events that ended N days ago, never a held
// person's (organiser, calendar owner, or guest of the event or of any of its
// instances), deleted ones included; the count shown before Save is what the
// sweep removes, and a rule sent with a count that has moved is refused with
// the count now; a removed event leaves the events search index, so no search
// finds its title, place or guests; one sweep writes ONE retention.swept row
// with its counts; and every hold change, search and export is a row in the
// audit log, saying what the change changed (one that changes nothing writes
// none).
//
// Run: go test -tags=integration ./tests/integration/ -run TestRetentionKeepsWhatAHoldHoldsOverHTTP -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// retentionSeries writes an ended weekly series organised by by, with one
// changed instance whose only guest is guest, and answers the series' id.
func retentionSeries(t *testing.T, db *sql.DB, by, guest uuid.UUID, start time.Time) uuid.UUID {
	t.Helper()
	first := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: by, Title: "Weekly vendor sync", Start: start, End: start.Add(time.Hour)})
	until := start.AddDate(0, 0, 21)
	if _, err := db.Exec(`UPDATE events SET rrule = 'FREQ=WEEKLY;COUNT=4', until_at = $2 WHERE id = $1`, first, until); err != nil {
		t.Fatal(err)
	}
	moved := start.AddDate(0, 0, 7)
	override := uuid.New()
	if _, err := db.Exec(`INSERT INTO events (id, calendar_id, kind, title, start_at, end_at, tz, created_by, ical_uid, series_id, recurrence_id)
		SELECT $1, calendar_id, 'event', 'Weekly vendor sync (moved)', $2::timestamptz + interval '2 hours', $2::timestamptz + interval '3 hours', 'UTC', created_by, ical_uid, id, $2
		FROM events WHERE id = $3`, override, moved, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO event_attendees (event_id, user_id) VALUES ($1, $2)`, override, guest); err != nil {
		t.Fatal(err)
	}
	return first
}

func TestRetentionKeepsWhatAHoldHoldsOverHTTP(t *testing.T) {
	t.Setenv("JWT_SECRET", "retention-holds-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	// Who may call is what the admin gate decides; CSRF has its own tests.
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
	// Every index, mapped as the server maps them at boot: an event a rule
	// removes must leave the events index with it.
	search := integration.SetupOpenSearch(t)
	if err := opensearchInit.ConnectOpenSearch(&opensearchInit.OpenSearchConfig{Host: search.URL}); err != nil {
		t.Fatalf("create the indices: %v", err)
	}

	person := func(email, name string) uuid.UUID {
		t.Helper()
		id, _, err := userBusiness.CreateUserWithMethod(ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ada := person("ada@retention.test", "Ada Admin")
	if err := userBusiness.CreateAdminUser(ctx, "ada@retention.test"); err != nil {
		t.Fatal(err)
	}
	maya, sam, lee := person("maya@retention.test", "Maya"), person("sam@retention.test", "Sam"), person("lee@retention.test", "Lee")

	routes := router.Routes()
	call := func(who uuid.UUID, method, path string, body any) (int, map[string]any, *httptest.ResponseRecorder) {
		t.Helper()
		token, err := helpers.SignSessionToken(who.String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
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
		return rec.Code, out, rec
	}
	preview := func(days int) float64 {
		t.Helper()
		code, out, _ := call(ada, http.MethodGet, "/admin/retention/rules/calendar/preview?days="+strconv.Itoa(days), nil)
		data, _ := out["data"].(map[string]any)
		n, ok := data["would_remove"].(float64)
		if code != http.StatusOK || !ok {
			t.Fatalf("preview: %d %v", code, out)
		}
		return n
	}
	auditCount := func(action string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	alive := func(id uuid.UUID) bool {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	old := time.Now().AddDate(-1, 0, -10).UTC().Truncate(time.Hour)
	samOld := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: sam, Title: "Sam's old one-to-one", Start: old, End: old.Add(time.Hour)})
	samGone := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: sam, Title: "Sam's old offsite", Start: old, End: old.Add(2 * time.Hour), Deleted: true})
	mayaOld := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: maya, Title: "Maya's supplier call", Start: old, End: old.Add(time.Hour)})
	mayaGone := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: maya, Title: "Maya's deleted pricing review", StartDate: old.Format("2006-01-02"), EndDate: old.AddDate(0, 0, 1).Format("2006-01-02"), Deleted: true})
	series := retentionSeries(t, db, sam, lee, old)
	recent := time.Now().AddDate(0, 0, -2).UTC().Truncate(time.Hour)
	samRecent := integration.InsertStoreEvent(t, db, integration.StoreEvent{By: sam, Title: "Sam's standup", Start: recent, End: recent.Add(time.Hour)})
	// Sam's ended one-to-one has a place and a guest from outside, each in its
	// search entry.
	if _, err := db.Exec(`UPDATE events SET location = 'Old Mill, room 7' WHERE id = $1`, samOld); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO event_attendees (event_id, email, name) VALUES ($1, 'priya@counsel.example', 'Priya Outsidecounsel')`, samOld); err != nil {
		t.Fatal(err)
	}

	// findable is how many entries of the events index hold text as a phrase
	// in a title, notes, place or guest's name, once every search write asked
	// for has run: the index under every reader's access, so 0 is no search
	// anywhere finding it.
	findable := func(t *testing.T, text string) int {
		t.Helper()
		helpers.SettleBackground(30 * time.Second)
		for deadline := time.Now().Add(30 * time.Second); ; {
			_ = outboxBusiness.Drain(ctx)
			var waiting int
			if err := db.QueryRow(`SELECT count(*) FROM outbox WHERE topic = 'calendar.search' AND (state IN ('held', 'ready') OR claimed_until > NOW())`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d search writes still waiting", waiting)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err := opensearchInit.RefreshIndices(ctx, openSearchStruct.EVENT_INDEX); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"query": map[string]any{"multi_match": map[string]any{
			"query": text, "type": "phrase", "fields": []string{"event_title", "event_notes", "event_location", "event_guest_names"}}}})
		resp, err := http.Post(search.URL+"/"+openSearchStruct.EVENT_INDEX+"/_count", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Count *int `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Count == nil {
			t.Fatalf("counting %q in the events index: %d %v", text, resp.StatusCode, err)
		}
		return *out.Count
	}
	// The entries as the nightly pass writes them: one each for the events not
	// in the trash.
	if err := calendarBusiness.WriteEverySearchEntry(ctx); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Sam's old one-to-one", "Old Mill", "Priya Outsidecounsel", "Maya's supplier call", "Weekly vendor sync", "Sam's standup"} {
		if n := findable(t, text); n != 1 {
			t.Fatalf("before any sweep the events index finds %q %d times, want 1", text, n)
		}
	}

	t.Run("a member who isn't an admin reaches none of it", func(t *testing.T) {
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/admin/retention/rules"},
			{http.MethodGet, "/admin/retention/rules/calendar/preview?days=30"},
			{http.MethodPut, "/admin/retention/rules/calendar"},
			{http.MethodGet, "/admin/retention/holds"},
			{http.MethodPost, "/admin/retention/holds"},
		} {
			if code, out, _ := call(sam, c.method, c.path, map[string]any{"days": 1, "shown": 0, "name": "x", "add": []string{sam.String()}}); code != http.StatusForbidden {
				t.Fatalf("%s %s as a member: %d %v, want 403", c.method, c.path, code, out)
			}
		}
		if n := auditCount("retention.hold_placed") + auditCount("policy.changed"); n != 0 {
			t.Fatalf("a refused request wrote %d rows", n)
		}
	})

	var holdID string
	t.Run("a hold is placed, changed, and holds from the moment it's placed", func(t *testing.T) {
		if n := preview(30); n != 5 {
			t.Fatalf("before any hold a 30-day rule would remove %v, want 5 (every ended event, deleted ones too)", n)
		}
		code, out, _ := call(ada, http.MethodPost, "/admin/retention/holds", map[string]any{"name": "Vendor dispute", "note": "Counsel's request", "add": []string{maya.String(), lee.String()}})
		data, _ := out["data"].(map[string]any)
		holdID, _ = data["id"].(string)
		if code != http.StatusCreated || holdID == "" {
			t.Fatalf("placing the hold: %d %v", code, out)
		}
		if n := preview(30); n != 2 {
			t.Fatalf("with Maya and Lee held a 30-day rule would remove %v, want 2 (Sam's own two)", n)
		}
		if code, out, _ := call(ada, http.MethodPatch, "/admin/retention/holds/"+holdID, map[string]any{"add": []string{sam.String()}}); code != http.StatusOK {
			t.Fatalf("adding Sam: %d %v", code, out)
		}
		if n := preview(30); n != 0 {
			t.Fatalf("with Sam held too it would remove %v, want 0", n)
		}
		// Sam twice and Ada, who isn't on it: only Sam comes off, and the row
		// says so.
		if code, out, _ := call(ada, http.MethodPatch, "/admin/retention/holds/"+holdID, map[string]any{"remove": []string{sam.String(), sam.String(), ada.String()}, "name": "Vendor dispute 2026"}); code != http.StatusOK {
			t.Fatalf("taking Sam off: %d %v", code, out)
		}
		lastChange := func() (summary, meta string) {
			t.Helper()
			if err := db.QueryRow(`SELECT summary, metadata::text FROM admin_audit_log WHERE action = 'retention.hold_changed' ORDER BY seq DESC LIMIT 1`).Scan(&summary, &meta); err != nil {
				t.Fatal(err)
			}
			return summary, meta
		}
		if summary, meta := lastChange(); summary != "Changed hold “Vendor dispute 2026”: renamed it from “Vendor dispute”, took off 1 person" ||
			!strings.Contains(meta, sam.String()) || strings.Contains(meta, ada.String()) {
			t.Errorf("taking Sam off is recorded as %q %s, want only Sam taken off", summary, meta)
		}
		if n := preview(30); n != 2 {
			t.Fatalf("with Sam off again it would remove %v, want 2", n)
		}
		if code, out, _ := call(ada, http.MethodPost, "/admin/retention/holds", map[string]any{"name": "Nobody", "add": []string{uuid.NewString()}}); code != http.StatusBadRequest {
			t.Fatalf("a hold naming nobody in the workspace: %d %v, want 400", code, out)
		}
		// Maya, who is on it, added; Sam, who isn't, taken off twice; nobody
		// taken off; its own name: nothing changes, so nothing is recorded and
		// the hold isn't stamped as changed.
		var stamped, restamped time.Time
		if err := db.QueryRow(`SELECT updated_at FROM retention_holds WHERE id = $1`, holdID).Scan(&stamped); err != nil {
			t.Fatal(err)
		}
		code, out, _ = call(ada, http.MethodPatch, "/admin/retention/holds/"+holdID, map[string]any{"name": "Vendor dispute 2026",
			"add": []string{maya.String()}, "remove": []string{sam.String(), sam.String(), uuid.Nil.String()}})
		data, _ = out["data"].(map[string]any)
		if people, _ := data["people"].([]any); code != http.StatusOK || len(people) != 2 {
			t.Fatalf("a change that changes nothing: %d %v, want 200 with Maya and Lee", code, out)
		}
		if err := db.QueryRow(`SELECT updated_at FROM retention_holds WHERE id = $1`, holdID).Scan(&restamped); err != nil || !restamped.Equal(stamped) {
			t.Errorf("a change that changed nothing stamped the hold %v (was %v): %v", restamped, stamped, err)
		}
		if got := auditCount("retention.hold_placed") + auditCount("retention.hold_changed"); got != 3 {
			t.Fatalf("the hold's place and two changes wrote %d rows, want 3 (a change that changes nothing writes none)", got)
		}
	})

	t.Run("a rule is saved only with the count it showed, and reads back", func(t *testing.T) {
		if code, out, _ := call(ada, http.MethodPut, "/admin/retention/rules/calendar", map[string]any{"days": 30}); code != http.StatusBadRequest {
			t.Fatalf("a rule without the count shown: %d %v, want 400", code, out)
		}
		// The panel showed 1 and it would remove 2 now: refused with the count
		// now, nothing recorded, the rule as it was.
		code, out, _ := call(ada, http.MethodPut, "/admin/retention/rules/calendar", map[string]any{"days": 30, "shown": 1})
		if data, _ := out["data"].(map[string]any); code != http.StatusConflict || data["would_remove"] != float64(2) {
			t.Fatalf("a rule sent with a count that moved (1 shown, 2 now): %d %v, want 409 with the count now", code, out)
		}
		if n := auditCount("policy.changed"); n != 0 {
			t.Fatalf("a refused rule wrote %d policy.changed rows", n)
		}
		days := func() any {
			t.Helper()
			code, out, _ := call(ada, http.MethodGet, "/admin/retention/rules", nil)
			data, _ := out["data"].(map[string]any)
			rules, _ := data["rules"].([]any)
			// Every kind lists its rule (calendar events, boards): this one
			// is the calendar's.
			for _, r := range rules {
				if rule, _ := r.(map[string]any); rule["kind"] == "calendar" {
					return rule["days"]
				}
			}
			t.Fatalf("the rules: %d %v, none for the calendar", code, out)
			return nil
		}
		if d := days(); d != float64(0) {
			t.Fatalf("after a refused rule the rule is %v days, want it still off", d)
		}
		if code, out, _ := call(ada, http.MethodPut, "/admin/retention/rules/calendar", map[string]any{"days": 30, "shown": 2}); code != http.StatusOK {
			t.Fatalf("saving the rule: %d %v", code, out)
		}
		if d := days(); d != float64(30) {
			t.Fatalf("the rule after saving 30 days: %v", d)
		}
		var meta string
		if err := db.QueryRow(`SELECT metadata::text FROM admin_audit_log WHERE action = 'policy.changed' ORDER BY seq DESC LIMIT 1`).Scan(&meta); err != nil ||
			!strings.Contains(meta, `"would_remove": 2`) || !strings.Contains(meta, `"shown": 2`) {
			t.Fatalf("the rule's record: %q %v", meta, err)
		}
	})

	t.Run("a hold's events are searched and exported, deleted ones and an instance's guest's included, each recorded", func(t *testing.T) {
		code, out, _ := call(ada, http.MethodGet, "/admin/retention/holds/"+holdID+"/items/calendar", nil)
		items, _ := out["data"].([]any)
		if code != http.StatusOK || len(items) != 3 {
			t.Fatalf("searching the hold: %d %v, want Maya's two and the series Lee is a guest of", code, out)
		}
		ids := map[string]bool{}
		for _, it := range items {
			ids[it.(map[string]any)["id"].(string)] = true
		}
		if !ids[mayaOld.String()] || !ids[mayaGone.String()] || !ids[series.String()] {
			t.Fatalf("the hold's items: %v", ids)
		}
		code, _, rec := call(ada, http.MethodGet, "/admin/retention/holds/"+holdID+"/export/calendar?format=json&q=pricing", nil)
		if code != http.StatusOK || rec.Header().Get("X-Exported-Items") != "1" || !strings.Contains(rec.Body.String(), "Maya's deleted pricing review") {
			t.Fatalf("the JSON export: %d %s %q", code, rec.Header().Get("X-Exported-Items"), rec.Body.String())
		}
		code, _, rec = call(ada, http.MethodGet, "/admin/retention/holds/"+holdID+"/export/calendar", nil)
		if code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/calendar") || !strings.Contains(rec.Body.String(), "BEGIN:VCALENDAR") {
			t.Fatalf("the default (iCalendar) export: %d %q", code, rec.Header().Get("Content-Type"))
		}
		if s, e := auditCount("retention.hold_searched"), auditCount("retention.hold_exported"); s != 1 || e != 2 {
			t.Fatalf("searched %d and exported %d rows, want 1 and 2", s, e)
		}
	})

	// V-388 (1): an export stopped at its cap with no sign, so an admin could
	// take the first 20,000 of a hold's events for all of them. Past
	// ExportPartSize an export answers in parts: each says so in its headers
	// and in its own file, offers the next, and is a row of its own.
	t.Run("an export past its part size says so in the answer and the file, and offers the rest", func(t *testing.T) {
		prev := retentionBusiness.ExportPartSize
		retentionBusiness.ExportPartSize = 2
		t.Cleanup(func() { retentionBusiness.ExportPartSize = prev })
		exportsBefore := auditCount("retention.hold_exported")
		events := func(out map[string]any) []string {
			list, _ := out["events"].([]any)
			ids := []string{}
			for _, e := range list {
				ids = append(ids, e.(map[string]any)["id"].(string))
			}
			return ids
		}
		base := "/admin/retention/holds/" + holdID + "/export/calendar?format=json"
		code, out, rec := call(ada, http.MethodGet, base, nil)
		next := rec.Header().Get("X-Export-Next")
		first := events(out)
		if code != http.StatusOK || rec.Header().Get("X-Exported-Items") != "2" || rec.Header().Get("X-Export-Part") != "1" || next == "" || len(first) != 2 {
			t.Fatalf("part 1: %d items=%q part=%q next=%q %v", code, rec.Header().Get("X-Exported-Items"), rec.Header().Get("X-Export-Part"), next, first)
		}
		if note, _ := out["note"].(string); out["part"] != float64(1) || out["more"] != true ||
			note != "Part 1 of this hold's export: the first 2 calendar events that match, not all of them. More remain: export part 2 from the hold's search for the rest." {
			t.Fatalf("part 1's file says %v %v %q", out["part"], out["more"], out["note"])
		}
		code, out, rec = call(ada, http.MethodGet, base+"&part="+next, nil)
		second := events(out)
		if code != http.StatusOK || rec.Header().Get("X-Exported-Items") != "1" || rec.Header().Get("X-Export-Part") != "2" || rec.Header().Get("X-Export-Next") != "" || len(second) != 1 {
			t.Fatalf("part 2: %d items=%q part=%q next=%q %v", code, rec.Header().Get("X-Exported-Items"), rec.Header().Get("X-Export-Part"), rec.Header().Get("X-Export-Next"), second)
		}
		if note, _ := out["note"].(string); out["part"] != float64(2) || out["more"] != false ||
			note != "Part 2 of this hold's export, the last: 1 more of the calendar events that match. Parts 1 to 2 hold all of them." {
			t.Fatalf("part 2's file says %v %v %q", out["part"], out["more"], out["note"])
		}
		all := map[string]bool{}
		for _, id := range append(first, second...) {
			all[id] = true
		}
		if len(all) != 3 || !all[mayaOld.String()] || !all[mayaGone.String()] || !all[series.String()] {
			t.Fatalf("parts 1 and 2 hold %v, want each of the hold's three events once", append(first, second...))
		}
		code, _, rec = call(ada, http.MethodGet, "/admin/retention/holds/"+holdID+"/export/calendar", nil)
		if code != http.StatusOK || rec.Header().Get("X-Export-Next") == "" || !strings.Contains(rec.Body.String(), "X-WR-CALDESC:Part 1 of this hold's export") {
			t.Fatalf("the iCalendar part 1: %d next=%q %q", code, rec.Header().Get("X-Export-Next"), rec.Body.String())
		}
		code, out, _ = call(ada, http.MethodGet, base+"&part=1.eA", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("a part the export never offered: %d %v, want 400", code, out)
		}
		// Well formed, but its cursor ("nope") isn't one the calendar writes.
		code, out, _ = call(ada, http.MethodGet, base+"&part=2.bm9wZQ", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("a part whose cursor the calendar never gave: %d %v, want 400", code, out)
		}
		if n := auditCount("retention.hold_exported"); n != exportsBefore+3 {
			t.Fatalf("exported rows %d, want %d: each part is a row", n, exportsBefore+3)
		}
		var summary, meta string
		if err := db.QueryRow(`SELECT summary, metadata::text FROM admin_audit_log WHERE action = 'retention.hold_exported' ORDER BY seq DESC LIMIT 1`).Scan(&summary, &meta); err != nil ||
			!strings.HasPrefix(summary, "Exported part 1 of hold ") || !strings.HasSuffix(summary, "as ICS (2; more remain)") || !strings.Contains(meta, `"part": 1`) || !strings.Contains(meta, `"more": true`) {
			t.Fatalf("the last part's record: %q %q %v", summary, meta, err)
		}
	})

	t.Run("the sweep removes exactly the count shown, keeps every held event, and writes one row", func(t *testing.T) {
		if err := retentionBusiness.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if alive(samOld) || alive(samGone) {
			t.Fatal("the sweep kept an ended event nobody held")
		}
		for name, id := range map[string]uuid.UUID{"Maya's": mayaOld, "Maya's deleted": mayaGone, "the series a held guest is on": series, "Sam's recent": samRecent} {
			if !alive(id) {
				t.Fatalf("the sweep removed %s event", name)
			}
		}
		var overrides int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE series_id = $1`, series).Scan(&overrides); err != nil || overrides != 1 {
			t.Fatalf("the held series' changed instance: %d %v", overrides, err)
		}
		var meta string
		if err := db.QueryRow(`SELECT metadata::text FROM admin_audit_log WHERE action = 'retention.swept'`).Scan(&meta); err != nil {
			t.Fatalf("the sweep's one row: %v", err)
		}
		if !strings.Contains(meta, `"calendar": 2`) {
			t.Fatalf("the sweep's row says %s, want 2 calendar events removed", meta)
		}
		// What it removed left search with it: its title, place and guest
		// are found nowhere, and the events it kept are found as before.
		for _, text := range []string{"Sam's old one-to-one", "Old Mill", "Priya Outsidecounsel"} {
			if n := findable(t, text); n != 0 {
				t.Errorf("after the sweep the events index still finds %q (%d entries): a removed event stays searchable", text, n)
			}
		}
		for _, text := range []string{"Maya's supplier call", "Weekly vendor sync", "Sam's standup"} {
			if n := findable(t, text); n != 1 {
				t.Errorf("after the sweep the events index finds the kept %q %d times, want 1", text, n)
			}
		}
	})

	t.Run("a released hold keeps its record, and its people's events fall under the rule again", func(t *testing.T) {
		if code, out, _ := call(ada, http.MethodPost, "/admin/retention/holds/"+holdID+"/release", nil); code != http.StatusOK {
			t.Fatalf("releasing: %d %v", code, out)
		}
		if code, _, _ := call(ada, http.MethodGet, "/admin/retention/holds/"+holdID+"/items/calendar", nil); code != http.StatusConflict {
			t.Fatalf("searching a released hold: %d, want 409", code)
		}
		if n := preview(30); n != 3 {
			t.Fatalf("after the release a 30-day rule would remove %v, want 3", n)
		}
		if err := retentionBusiness.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if alive(mayaOld) || alive(mayaGone) || alive(series) || !alive(samRecent) {
			t.Fatal("the second sweep didn't remove exactly the released events")
		}
		for text, want := range map[string]int{"Maya's supplier call": 0, "Weekly vendor sync": 0, "Sam's standup": 1} {
			if n := findable(t, text); n != want {
				t.Errorf("after the second sweep the events index finds %q %d times, want %d", text, n, want)
			}
		}
		if n := auditCount("retention.swept"); n != 2 {
			t.Fatalf("two sweeps wrote %d rows, want 2", n)
		}
		var people int
		if err := db.QueryRow(`SELECT count(*) FROM retention_hold_people WHERE hold_id = $1`, holdID).Scan(&people); err != nil || people != 3 {
			t.Fatalf("the released hold's people rows: %d %v, want 3 (Maya, Lee, and Sam's added and taken off)", people, err)
		}
		res, err := auditBusiness.Verify(ctx)
		if err != nil || res == nil || !res.OK {
			t.Fatalf("the audit chain doesn't verify: %+v %v", res, err)
		}
	})
}
