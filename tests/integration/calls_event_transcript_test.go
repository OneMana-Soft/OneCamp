//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// An event's call that nobody recorded keeps its words on a transcript-only
// node, and one the boot repair ended (MarkCallSessionNodeEnded) has no start
// time. Whoever may change the event deletes it through the event's own route
// and brings it back (its Undo), as they do a recording: its node is stamped,
// then cleared (V-140; CR-118's ASK 2). Before V-140 the write looked only for
// a node with a start time, so the delete answered 200 and changed nothing.
func TestAnEventsTranscriptWithNoStartTimeIsDeletedAndBroughtBack(t *testing.T) {
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
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("its call: %d %v", code, body)
	}

	// The node as an older call left it, filed under the event with no start
	// and no file; then the boot repair ends it, flagging it transcript-only.
	key := "call-RM_eventtranscript"
	dg.Mutate(t, map[string]any{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": key, "recording_event": e.ID})
	if err := recordingDomain.MarkCallSessionNodeEnded(ctx, key, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	deletedAt := func() string {
		var kept struct {
			Q []struct {
				DeletedAt      string `json:"recording_deleted_at"`
				TranscriptOnly bool   `json:"recording_transcript_only"`
				StartedAt      string `json:"recording_stared_at"`
			} `json:"q"`
		}
		q := `{ q(func: eq(recording_egress_id, "` + key + `")) { recording_deleted_at recording_transcript_only recording_stared_at } }`
		if err := json.Unmarshal(dg.Query(t, q), &kept); err != nil || len(kept.Q) != 1 {
			t.Fatalf("the transcript's node: %+v, %v; want it, and only it", kept.Q, err)
		}
		if n := kept.Q[0]; !n.TranscriptOnly || n.StartedAt != "" {
			t.Fatalf("the node = %+v, want a transcript-only one with no start time", n)
		}
		return kept.Q[0].DeletedAt
	}
	change := func(who *userModels.UserInfo, restore bool) int {
		method, path, handler := http.MethodDelete, "/calendar/events/"+e.ID+"/call/artefacts/"+key, eventController.DeleteCalendarEventCallArtefact
		if restore {
			method, path, handler = http.MethodPost, path+"/restore", eventController.RestoreCalendarEventCallArtefact
		}
		return call(t, handler, method, path, who, "", map[string]string{"eventId": e.ID, "key": key}).Code
	}

	if at := deletedAt(); at >= "1971" {
		t.Fatalf("the transcript starts out deleted at %q", at)
	}
	if code := change(maya, false); code != http.StatusForbidden {
		t.Errorf("the member it invites, who may not change the event, deletes its transcript: %d, want 403", code)
	}
	if code := change(sam, false); code != http.StatusOK {
		t.Fatalf("its organiser deletes the transcript: %d, want 200", code)
	}
	if at := deletedAt(); at < "1971" {
		t.Errorf("the deleted transcript's node says deleted at %q, want the time it was deleted", at)
	}
	if code := change(sam, false); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code := change(sam, true); code != http.StatusOK {
		t.Fatalf("its organiser's Undo: %d, want 200", code)
	}
	if at := deletedAt(); at >= "1971" {
		t.Errorf("the transcript brought back still says deleted at %q", at)
	}
}

// Whoever may read an event reads its call's transcript through the event's
// own route (meet-02, head C2), a page at a time, in the order its lines were
// said, as the player pages a conversation's transcript: the member it
// invites does, and a member it doesn't invite is told there is no such
// event. A transcript filed under another event isn't read through this one,
// and a deleted one is gone until its Undo.
func TestWhoeverMayReadAnEventReadsItsCallsTranscript(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jo := seedCalendarPerson(t, env.PG, dg, "Jo")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	event := func(title string, hour int) string {
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"`+title+`","start":{"at":"`+at(hour)+`","zone":"Europe/London"},"end":{"at":"`+at(hour+1)+`"},`+
				`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", title, code, body)
		}
		e := dataEvent(t, body)
		if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
			t.Fatalf("%s's call: %d %v", title, code, body)
		}
		return e.ID
	}
	review, standup := event("Design review", 9), event("Standup", 11)

	// Each call's words, filed under its event as a call that nobody recorded
	// files them: a transcript-only node, its lines said by Maya, out of order.
	said := time.Now().Add(-time.Hour).UnixMilli()
	filed := func(key, event string, lines ...string) {
		node := map[string]any{"uid": "_:rec", "dgraph.type": "Recording", "recording_egress_id": key, "recording_event": event,
			"recording_transcript_only": true, "recording_stared_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}
		var kept []map[string]any
		for i := len(lines) - 1; i >= 0; i-- {
			kept = append(kept, map[string]any{"dgraph.type": "Transcript", "transcript_text": lines[i],
				"transcript_timestamp": said + int64(i)*1000, "transcript_from": map[string]any{"uid": maya.UserDgraphInfo.Uid}})
		}
		node["recording_transcript"] = kept
		dg.Mutate(t, node)
	}
	filed("call-RM_review", review, "Launch is Thursday.", "The rollback steps go in the notes.", "Legal wants them in writing.")
	filed("call-RM_standup", standup, "Nothing blocks me.")

	type page struct {
		Data struct {
			Lines []struct {
				Text string `json:"transcript_text"`
				From struct {
					Name string `json:"user_name"`
				} `json:"transcript_from"`
			} `json:"recording_transcript"`
			Count int `json:"recording_transcript_count"`
		} `json:"data"`
		PageCount int `json:"pageCount"`
	}
	read := func(who *userModels.UserInfo, event, key, query string) (int, page) {
		w := call(t, eventController.GetCalendarEventCallTranscript, http.MethodGet,
			"/calendar/events/"+event+"/call/artefacts/"+key+"/transcript"+query, who, "", map[string]string{"eventId": event, "key": key})
		var out page
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	texts := func(p page) (out []string) {
		for _, l := range p.Data.Lines {
			out = append(out, l.Text)
		}
		return out
	}

	code, first := read(maya, review, "call-RM_review", "?pageIndex=0&pageSize=2")
	if code != http.StatusOK || first.Data.Count != 3 || first.PageCount != 2 {
		t.Fatalf("the member it invites reads its first page: %d, %d lines in %d pages; want 200, 3 lines in 2 pages", code, first.Data.Count, first.PageCount)
	}
	if got := texts(first); len(got) != 2 || got[0] != "Launch is Thursday." || got[1] != "The rollback steps go in the notes." {
		t.Errorf("the first page = %q, want its first two lines in the order they were said", got)
	}
	if first.Data.Lines[0].From.Name != "Maya" {
		t.Errorf("the first line is said by %q, want Maya", first.Data.Lines[0].From.Name)
	}
	if code, second := read(maya, review, "call-RM_review", "?pageIndex=1&pageSize=2"); code != http.StatusOK || len(texts(second)) != 1 || texts(second)[0] != "Legal wants them in writing." {
		t.Errorf("the second page: %d %q, want 200 and its last line", code, texts(second))
	}
	if code, _ := read(sam, review, "call-RM_review", ""); code != http.StatusOK {
		t.Errorf("its organiser reads it: %d, want 200", code)
	}
	if code, _ := read(jo, review, "call-RM_review", ""); code != http.StatusNotFound {
		t.Errorf("a member it doesn't invite reads it: %d, want 404", code)
	}
	if code, _ := read(maya, review, "call-RM_standup", ""); code != http.StatusNotFound {
		t.Errorf("another event's transcript through this one: %d, want 404", code)
	}
	del := call(t, eventController.DeleteCalendarEventCallArtefact, http.MethodDelete, "/calendar/events/"+review+"/call/artefacts/call-RM_review",
		sam, "", map[string]string{"eventId": review, "key": "call-RM_review"})
	if del.Code != http.StatusOK {
		t.Fatalf("its organiser deletes it: %d, want 200", del.Code)
	}
	if code, _ := read(maya, review, "call-RM_review", ""); code != http.StatusNotFound {
		t.Errorf("a deleted transcript: %d, want 404", code)
	}
}
