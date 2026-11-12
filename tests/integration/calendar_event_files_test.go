//go:build integration

package integration_test

// An event's files, the trash's 30 days and an event's own colour over HTTP
// (calendar gcal-1a head B), against real Postgres 12, Dgraph and Redis:
//
//   - its organiser adds a doc and a board, reads them, takes one off and
//     adds it again, which brings the same row back; a guest who may not
//     change the event is refused, and so is a doc the adder can't open;
//     the 26th is refused in words; a new event made with a doc its writer
//     can't open is refused 403 in the same words, and nothing is made;
//   - a reader sees each file they may open by kind and id only, never a
//     title, and nothing of one they can't (no id, no count: the permission
//     model, MAIN 13:4x 11 Nov), and names it through /entities, where a
//     doc they can't open answers "none";
//   - someone who sees only that the organiser is busy, and a private
//     event's busy view, carry no files;
//   - each change is told live to the event's readers alone;
//   - "this and following" carries the series' files onto the new series;
//   - the trash lists what was deleted 29 days ago, not 31, with who
//     deleted it, and never another member's;
//   - a hex colour is kept and read back, and a bad one is refused in words;
//     Your colours keep the newest twelve.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestAnEventsFilesOverHTTP|TestTheTrashAndAnEventsOwnColourOverHTTP|TestCalendarEventFilesMigrationUpDownUp)$' -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/akashc777/OneCamp/business/Board" // the board read rule
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	_ "github.com/akashc777/OneCamp/business/Doc" // the doc read rule
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

// filesIn is an event's files as its JSON carries them: every key of each.
func filesIn(t *testing.T, body map[string]json.RawMessage) []map[string]string {
	t.Helper()
	var e struct {
		Files []map[string]string `json:"files"`
	}
	if err := json.Unmarshal(body["data"], &e); err != nil {
		t.Fatalf("no event in %v: %v", body, err)
	}
	return e.Files
}

func fileList(files []map[string]string) string {
	var out []string
	for _, f := range files {
		keys := make([]string, 0, len(f))
		for k := range f {
			keys = append(keys, k)
		}
		if len(keys) != 2 || f["kind"] == "" || f["uuid"] == "" {
			return fmt.Sprintf("a file with keys %v", keys)
		}
		out = append(out, f["kind"]+":"+f["uuid"])
	}
	return strings.Join(out, ",")
}

func TestAnEventsFilesOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	rec := &dg10Recorder{Client: MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1"))}
	integration.UseMqttClient(t, rec)
	ctx := context.Background()
	// Sam organises; Maya is his guest, who may not change it; Bea is a
	// member who sees only when Sam is busy; Pat sees Sam's calendar's
	// details, by a share.
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	bea := seedCalendarPerson(t, env.PG, dg, "Bea")
	pat := seedCalendarPerson(t, env.PG, dg, "Pat")
	const live = "0001-01-01T00:00:00Z"
	brief, samsOwn, beasSecret, retro, handbook := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	user := func(u *userModels.UserInfo) map[string]any { return map[string]any{"uid": u.UserDgraphInfo.Uid} }
	dg.Mutate(t, []map[string]any{
		{"dgraph.type": "Doc", "doc_uuid": brief, "doc_title": "Launch brief", "doc_private": true, "doc_deleted_at": live,
			"doc_created_by": user(sam), "doc_reading_users": []map[string]any{user(maya)}},
		{"dgraph.type": "Doc", "doc_uuid": samsOwn, "doc_title": "Sam's notes", "doc_private": true, "doc_deleted_at": live,
			"doc_created_by": user(sam)},
		{"dgraph.type": "Doc", "doc_uuid": beasSecret, "doc_title": "Bea's secret", "doc_private": true, "doc_deleted_at": live,
			"doc_created_by": user(bea)},
		{"dgraph.type": "Doc", "doc_uuid": handbook, "doc_title": "Handbook", "doc_private": false, "doc_deleted_at": live,
			"doc_created_by": user(bea)},
		{"dgraph.type": "Board", "board_uuid": retro, "board_title": "Retro", "board_private": false, "board_deleted_at": live,
			"board_created_by": user(sam)},
	})

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","visibility":"private","start":{"at":"`+at(9)+`","zone":"UTC"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
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
	rows := func(kind, id string) (n int, live int) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT count(*), count(*) FILTER (WHERE deleted_at IS NULL) FROM calendar_event_files
			WHERE event_id = $1 AND ref_kind = $2 AND ref_uuid = $3`, e.ID, kind, id).Scan(&n, &live); err != nil {
			t.Fatal(err)
		}
		return n, live
	}
	liveTo := func() []string {
		t.Helper()
		if !resourceEventBusiness.Sinks.Settle(30 * time.Second) {
			t.Fatal("the sinks didn't settle")
		}
		time.Sleep(300 * time.Millisecond)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		var topics []string
		for _, s := range rec.sent {
			if s.msg.Type == mqttStruct.MESSAGE_CALENDAR_LIVE {
				topics = append(topics, s.topic)
			}
		}
		rec.sent = nil
		return topics
	}
	topic := func(u *userModels.UserInfo) string { return helpers.GetMqttTopicForUserActivity(u.UserDgraphInfo.Uuid) }
	liveTo()

	t.Run("its organiser adds a doc and a board, each once", func(t *testing.T) {
		if code, body := add(sam, "doc", strings.ToUpper(brief)); code != http.StatusOK || fileList(filesIn(t, body)) != "doc:"+brief {
			t.Fatalf("the brief: %d %v", code, body)
		}
		if code, body := add(sam, "doc", brief); code != http.StatusOK || fileList(filesIn(t, body)) != "doc:"+brief {
			t.Errorf("the brief again: %d %v", code, body)
		}
		for _, f := range [][2]string{{"doc", samsOwn}, {"board", retro}} {
			if code, body := add(sam, f[0], f[1]); code != http.StatusOK {
				t.Fatalf("%s: %d %v", f[0], code, body)
			}
		}
		if n, l := rows("doc", brief); n != 1 || l != 1 {
			t.Errorf("the brief's rows: %d, %d live", n, l)
		}
	})

	t.Run("each change is told live to the event's readers alone", func(t *testing.T) {
		got := liveTo()
		for _, who := range []*userModels.UserInfo{sam, maya} {
			if !containsString(got, topic(who)) {
				t.Errorf("%s wasn't told: %v", who.UserDgraphInfo.UserName, got)
			}
		}
		for _, who := range []*userModels.UserInfo{bea, pat} {
			if containsString(got, topic(who)) {
				t.Errorf("%s was told of an event they don't read: %v", who.UserDgraphInfo.UserName, got)
			}
		}
	})

	t.Run("a guest reads the files they may open by id, and nothing of the rest", func(t *testing.T) {
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, maya, "", e.ID)
		if got := fileList(filesIn(t, body)); code != http.StatusOK || got != "doc:"+brief+",board:"+retro {
			t.Fatalf("Maya reads %d %s", code, got)
		}
		if strings.Contains(string(body["data"]), "Launch brief") || strings.Contains(string(body["data"]), "Sam's notes") {
			t.Error("the event carries a file's title")
		}
		if strings.Contains(string(body["data"]), samsOwn) {
			t.Error("the event carries the id of a doc Maya can't open")
		}
		raw, _ := json.Marshal(map[string]any{"refs": []map[string]string{{"kind": "doc", "id": brief}, {"kind": "doc", "id": samsOwn}, {"kind": "board", "id": retro}}})
		chips := call(t, entityLinkController.ReadEntities, http.MethodPost, "/entities", maya, string(raw), nil)
		var out struct {
			Data []struct {
				Kind, ID, Access string
				Data             map[string]any
			} `json:"data"`
		}
		if chips.Code != http.StatusOK || json.Unmarshal(chips.Body.Bytes(), &out) != nil || len(out.Data) != 3 {
			t.Fatalf("Maya's chips: %d %s", chips.Code, chips.Body.String())
		}
		for _, c := range out.Data {
			switch c.ID {
			case brief:
				if c.Access == "none" || c.Data["title"] != "Launch brief" {
					t.Errorf("the brief, which Maya may read: %+v", c)
				}
			case samsOwn:
				if c.Access != "none" || c.Data["title"] != nil {
					t.Errorf("Sam's own doc, which Maya can't open: %+v", c)
				}
			}
		}
	})

	t.Run("a guest who may not change it, and a doc the adder can't open, are refused", func(t *testing.T) {
		if code, body := add(maya, "doc", brief); code != http.StatusForbidden {
			t.Errorf("Maya added a file: %d %v", code, body)
		}
		if code, body := remove(maya, "doc", brief); code != http.StatusForbidden {
			t.Errorf("Maya took a file off: %d %v", code, body)
		}
		if code, body := add(sam, "doc", beasSecret); code != http.StatusForbidden || !strings.Contains(string(body["msg"]), "open") {
			t.Errorf("a doc Sam can't open: %d %v", code, body)
		}
		if code, body := add(sam, "project", brief); code != http.StatusBadRequest {
			t.Errorf("a project: %d %v", code, body)
		}
		if code, body := add(bea, "doc", brief); code != http.StatusNotFound {
			t.Errorf("Bea, not on it: %d %v", code, body)
		}
		if n, _ := rows("doc", beasSecret); n != 0 {
			t.Error("a refused file has a row")
		}
	})

	// A duplicate, or a form that picks its files before it saves, sends them
	// with the new event: one its writer can't open is the same refusal as
	// adding it, never a fault to try again, and nothing is made.
	t.Run("a new event made with a doc its writer can't open is refused in words, and nothing is made", func(t *testing.T) {
		const title = "Copy of Design review, with Bea's secret"
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"`+title+`","start":{"at":"`+at(11)+`","zone":"UTC"},"end":{"at":"`+at(12)+`"},`+
				`"files":[{"kind":"doc","uuid":"`+brief+`"},{"kind":"doc","uuid":"`+beasSecret+`"}]}`, "")
		var msg string
		_ = json.Unmarshal(body["msg"], &msg)
		raw, _ := json.Marshal(body)
		t.Logf("PROBE create with a doc Sam can't open: STATUS=%d BODY=%s", code, raw)
		if code != http.StatusForbidden || msg != "You can add only a doc, a board or a task you can open." {
			t.Errorf("made with a doc Sam can't open: %d %q", code, msg)
		}
		var made, files int
		if err := env.PG.QueryRow(`SELECT count(*) FROM events WHERE title = $1`, title).Scan(&made); err != nil {
			t.Fatal(err)
		}
		if err := env.PG.QueryRow(`SELECT count(*) FROM calendar_event_files WHERE ref_uuid = $1`, beasSecret).Scan(&files); err != nil {
			t.Fatal(err)
		}
		if made != 0 || files != 0 {
			t.Errorf("a refused create made %d events and %d file rows", made, files)
		}
	})

	t.Run("taken off and added again, it is the same row", func(t *testing.T) {
		code, body := remove(sam, "doc", brief)
		if got := fileList(filesIn(t, body)); code != http.StatusOK || got != "doc:"+samsOwn+",board:"+retro {
			t.Fatalf("taken off: %d %s", code, got)
		}
		if n, l := rows("doc", brief); n != 1 || l != 0 {
			t.Errorf("taken off, the brief's rows: %d, %d live", n, l)
		}
		if got := liveTo(); !containsString(got, topic(maya)) || containsString(got, topic(bea)) {
			t.Errorf("told of the removal: %v", got)
		}
		if code, body := add(sam, "doc", brief); code != http.StatusOK || !strings.Contains(fileList(filesIn(t, body)), brief) {
			t.Fatalf("added again: %d %v", code, body)
		}
		if n, l := rows("doc", brief); n != 1 || l != 1 {
			t.Errorf("added again, the brief's rows: %d, %d live", n, l)
		}
	})

	t.Run("an event carries 25 files at most, said in words", func(t *testing.T) {
		for i := 0; i < 22; i++ {
			if _, err := env.PG.Exec(`INSERT INTO calendar_event_files (id, event_id, ref_kind, ref_uuid, added_by) VALUES ($1, $2, 'doc', $3, $4)`,
				uuid.New(), e.ID, uuid.New(), sam.UserPostgresInfo.Id); err != nil {
				t.Fatal(err)
			}
		}
		if code, body := add(sam, "doc", handbook); code != http.StatusBadRequest || !strings.Contains(string(body["msg"]), "25") {
			t.Errorf("a 26th: %d %v", code, body)
		}
		if code, body := add(sam, "board", retro); code != http.StatusOK {
			t.Errorf("one already on it, at 25: %d %v", code, body)
		}
		if code, body := remove(sam, "board", retro); code != http.StatusOK {
			t.Fatalf("one off: %d %v", code, body)
		}
		if code, body := add(sam, "doc", handbook); code != http.StatusOK {
			t.Errorf("room for one again: %d %v", code, body)
		}
		if code, body := add(sam, "board", retro); code != http.StatusBadRequest {
			t.Errorf("full again, the board back: %d %v", code, body)
		}
		if _, err := env.PG.Exec(`UPDATE calendar_event_files SET deleted_at = NOW(), deleted_by = $2
			WHERE event_id = $1 AND ref_uuid <> ALL($3::uuid[]) AND deleted_at IS NULL`, e.ID, sam.UserPostgresInfo.Id,
			"{"+brief+","+samsOwn+"}"); err != nil {
			t.Fatal(err)
		}
		if code, body := add(sam, "board", retro); code != http.StatusOK {
			t.Fatalf("the board back: %d %v", code, body)
		}
	})

	t.Run("a busy view and a private event's busy view carry no files", func(t *testing.T) {
		samCal, err := calendarBusiness.Events.PersonCalendar(ctx, bea, sam.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := calendarBusiness.Events.Share(ctx, sam, samCal.String(), pat.UserDgraphInfo.Uuid, models.ShareDetails); err != nil {
			t.Fatal(err)
		}
		// Sam's open event with a file: Pat, who sees its details, reads it.
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"Open hours","start":{"at":"`+at(13)+`","zone":"UTC"},"end":{"at":"`+at(14)+`"},`+
				`"files":[{"kind":"board","uuid":"`+retro+`"}]}`, "")
		if code != http.StatusCreated || fileList(filesIn(t, body)) != "board:"+retro {
			t.Fatalf("an open event made with a file: %d %v", code, body)
		}
		open := dataEvent(t, body)
		q := "/calendar/calendars/events?ids=" + samCal.String() + "&from=" + day.Format(time.RFC3339) + "&to=" + day.Add(24*time.Hour).Format(time.RFC3339)
		for _, c := range []struct {
			who        *userModels.UserInfo
			want, open string
		}{{bea, "", ""}, {pat, "", "board:" + retro}} {
			req := call(t, eventController.ListEventsOnCalendars, http.MethodGet, q, c.who, "", nil)
			var out struct {
				Data []struct {
					ID    string              `json:"id"`
					Title string              `json:"title"`
					Files []map[string]string `json:"files"`
				} `json:"data"`
			}
			if req.Code != http.StatusOK || json.Unmarshal(req.Body.Bytes(), &out) != nil {
				t.Fatalf("%s's look: %d %s", c.who.UserDgraphInfo.UserName, req.Code, req.Body.String())
			}
			seen := map[string]string{}
			for _, ev := range out.Data {
				if ev.ID == e.ID && ev.Title != "Busy" {
					t.Errorf("%s sees the private review as %q", c.who.UserDgraphInfo.UserName, ev.Title)
				}
				seen[ev.ID] = fileList(ev.Files)
			}
			if got, ok := seen[e.ID]; !ok || got != c.want {
				t.Errorf("%s: the private review's files %q (listed %v)", c.who.UserDgraphInfo.UserName, got, ok)
			}
			if got := seen[open.ID]; got != c.open {
				t.Errorf("%s: the open event's files %q, want %q", c.who.UserDgraphInfo.UserName, got, c.open)
			}
		}
	})

	// "This and following" makes a new series in one transaction, and the
	// series' files go along: the same rows, by the same person, at the same
	// time; the part that ended keeps its own.
	t.Run("this and following carries the series' files", func(t *testing.T) {
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"Weekly sync","start":{"at":"`+at(15)+`","zone":"UTC"},"end":{"at":"`+at(16)+`"},`+
				`"recurrence":{"rule":"FREQ=WEEKLY;COUNT=4"},"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}],`+
				`"files":[{"kind":"doc","uuid":"`+brief+`"},{"kind":"board","uuid":"`+retro+`"}]}`, "")
		if code != http.StatusCreated {
			t.Fatalf("the series: %d %v", code, body)
		}
		series := dataEvent(t, body)
		from, to := day.Format(time.RFC3339), day.AddDate(0, 0, 29).Format(time.RFC3339)
		weekly := func() []calendarBusiness.Event {
			var out []calendarBusiness.Event
			for _, ev := range apiList(t, maya, from, to) {
				if ev.Title == "Weekly sync" {
					out = append(out, ev)
				}
			}
			return out
		}
		in := weekly()
		if len(in) != 4 {
			t.Fatalf("%d instances", len(in))
		}
		code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+in[2].ID, sam,
			`{"sequence":`+itoa(in[2].Sequence)+`,"scope":"following","location":"Room 2"}`, in[2].ID)
		if code != http.StatusOK {
			t.Fatalf("this and following: %d %v", code, body)
		}
		fresh := dataEvent(t, body)
		want := "doc:" + brief + ",board:" + retro
		if fresh.ID == series.ID || fileList(filesIn(t, body)) != want {
			t.Fatalf("the new series: %s with %q", fresh.ID, fileList(filesIn(t, body)))
		}
		for _, ev := range weekly() {
			var got []string
			for _, f := range ev.Files {
				got = append(got, f.Kind+":"+f.UUID)
			}
			if strings.Join(got, ",") != want {
				t.Errorf("Maya reads %s with %v", ev.ID, got)
			}
		}
		var same int
		if err := env.PG.QueryRow(`SELECT count(*) FROM calendar_event_files n JOIN calendar_event_files o
			ON o.event_id = $1 AND n.event_id = $2 AND o.ref_kind = n.ref_kind AND o.ref_uuid = n.ref_uuid
			   AND o.added_by = n.added_by AND o.added_at = n.added_at AND o.deleted_at IS NULL AND n.deleted_at IS NULL`,
			series.ID, fresh.ID).Scan(&same); err != nil || same != 2 {
			t.Errorf("the new series' rows matching the series': %d %v", same, err)
		}
	})
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestTheTrashAndAnEventsOwnColourOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	newEvent := func(who *userModels.UserInfo, title, hue string) (int, map[string]json.RawMessage) {
		t.Helper()
		return v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", who,
			`{"title":"`+title+`","hue":"`+hue+`","start":{"at":"`+day.Add(9*time.Hour).Format(time.RFC3339)+`","zone":"UTC"},`+
				`"end":{"at":"`+day.Add(10*time.Hour).Format(time.RFC3339)+`"}}`, "")
	}
	made := func(who *userModels.UserInfo, title string) calendarBusiness.Event {
		t.Helper()
		code, body := newEvent(who, title, "")
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %v", title, code, body)
		}
		return dataEvent(t, body)
	}

	t.Run("the trash lists the last 30 days, with who deleted each", func(t *testing.T) {
		old, recent, mine := made(sam, "Old offsite"), made(sam, "Board prep"), made(maya, "Maya's own")
		for _, d := range []struct {
			who *userModels.UserInfo
			id  string
			ago string
		}{{sam, old.ID, "31 days"}, {sam, recent.ID, "29 days"}, {maya, mine.ID, "1 day"}} {
			if code, body := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+d.id, d.who, "", d.id); code != http.StatusOK && code != http.StatusNoContent {
				t.Fatalf("delete: %d %v", code, body)
			}
			if _, err := env.PG.Exec(`UPDATE events SET deleted_at = NOW() - $2::interval WHERE id = $1`, d.id, d.ago); err != nil {
				t.Fatal(err)
			}
		}
		code, body := v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash", sam, "", "")
		var trash []calendarBusiness.Event
		if code != http.StatusOK || json.Unmarshal(body["data"], &trash) != nil {
			t.Fatalf("Sam's trash: %d %v", code, body)
		}
		if len(trash) != 1 || trash[0].ID != recent.ID || trash[0].DeletedAt == nil || trash[0].DeletedBy == nil ||
			trash[0].DeletedBy.UUID != sam.UserDgraphInfo.Uuid {
			t.Errorf("Sam's trash: %+v", trash)
		}
		var deleted bool
		if err := env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM events WHERE id = $1`, old.ID).Scan(&deleted); err != nil || !deleted {
			t.Errorf("the 31-day-old one: deleted %v %v; nothing is purged", deleted, err)
		}
		code, body = v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash", maya, "", "")
		if code != http.StatusOK || json.Unmarshal(body["data"], &trash) != nil || len(trash) != 1 || trash[0].ID != mine.ID {
			t.Errorf("Maya's trash: %d %+v", code, trash)
		}
	})

	t.Run("an event's own colour is kept, and a bad one refused in words", func(t *testing.T) {
		code, body := newEvent(sam, "Launch", "#1a73e8")
		if code != http.StatusCreated {
			t.Fatalf("in #1a73e8: %d %v", code, body)
		}
		e := dataEvent(t, body)
		if code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+e.ID, sam, "", e.ID); code != http.StatusOK || dataEvent(t, body).Hue != "#1a73e8" {
			t.Errorf("read back: %d %v", code, body)
		}
		code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam,
			fmt.Sprintf(`{"sequence":%d,"hue":"#1A73E8"}`, e.Sequence), e.ID)
		if code != http.StatusBadRequest || !strings.Contains(string(body["msg"]), "#") {
			t.Errorf("#1A73E8: %d %v", code, body)
		}
		if code, body := newEvent(sam, "Launch", "crimson"); code != http.StatusBadRequest {
			t.Errorf("crimson: %d %v", code, body)
		}
	})

	t.Run("your colours keep the newest twelve", func(t *testing.T) {
		var list []string
		for i := 13; i >= 1; i-- {
			list = append(list, fmt.Sprintf("#0000%02x", i))
		}
		raw, _ := json.Marshal(list)
		if code, body := setPref(t, sam, calendarBusiness.PrefRecentColours, string(raw)); code != http.StatusOK {
			t.Fatalf("13 colours: %d %s", code, body)
		}
		var kept []string
		if err := json.Unmarshal(prefs(t, sam)[calendarBusiness.PrefRecentColours], &kept); err != nil || len(kept) != 12 || kept[0] != "#00000d" || kept[11] != "#000002" {
			t.Errorf("kept %v %v", kept, err)
		}
		if code, body := setPref(t, sam, calendarBusiness.PrefRecentColours, `["sky"]`); code != http.StatusBadRequest {
			t.Errorf("a name: %d %s", code, body)
		}
	})
}

// The files' migration goes up, down and up again: the table and its one
// live row per event and thing.
func TestCalendarEventFilesMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_calendar_event_files.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the files' migration: %v %v", matches, err)
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
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		var table, unique bool
		if err := env.PG.QueryRow(`SELECT to_regclass('calendar_event_files') IS NOT NULL,
			EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'calendar_event_files' AND indexdef LIKE '%UNIQUE%' AND indexdef LIKE '%deleted_at IS NULL%')`).Scan(&table, &unique); err != nil {
			t.Fatal(err)
		}
		if table != step.want || unique != step.want {
			t.Errorf("step %d: table %v, live index %v, want %v", i, table, unique, step.want)
		}
	}
}
