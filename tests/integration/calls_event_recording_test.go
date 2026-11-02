//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/akashc777/OneCamp/business/CallRoom"  // hands the calendar its event calls
	_ "github.com/akashc777/OneCamp/business/CallState" // hands business/LiveKit the event of a room's call
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	"github.com/akashc777/OneCamp/controllers/httpx"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/livekit/protocol/livekit"
)

// An event's call is recorded by its organiser, through the one start every
// call uses (head D): the recording is filed under the event, the event's
// "Recordings & notes" say the call was recorded, and whoever may read the
// event plays it. Nobody else starts it: not the member it invites, not a
// member it doesn't; and a member it doesn't invite never plays it.
func TestAnEventsOrganiserRecordsItsCallOntoTheEvent(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	// Playing answers with the recording's link, which storage signs.
	integration.SetupMinio(t, "event-call-recording-test")
	lk := integration.SetupLiveKit(t)
	egress := lk.UseFakeEgress(t)
	ctx := context.Background()
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Quarterly review","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("its call: %d %v", code, body)
	}
	var room string
	if err := env.PG.QueryRowContext(ctx, `SELECT call_room FROM events WHERE id = $1`, e.ID).Scan(&room); err != nil || !strings.HasPrefix(room, "meet-") {
		t.Fatalf("the event's call room = %q, %v", room, err)
	}
	if _, err := livekitInit.LiveKitService.CreateRoom(ctx, room); err != nil {
		t.Fatal(err)
	}
	meeting := strings.TrimPrefix(room, "meet-")
	record := func(who *userModels.UserInfo, on bool) int {
		body := `{"on":false}`
		if on {
			body = `{"on":true}`
		}
		return call(t, callsController.SetMeetingRecording, http.MethodPost, "/calls/meetings/"+meeting+"/recording", who, body,
			map[string]string{"meeting_id": meeting}).Code
	}

	for name, who := range map[string]*userModels.UserInfo{"the member it invites": maya, "a member it doesn't invite": lee} {
		if code := record(who, true); code != http.StatusNotFound {
			t.Errorf("%s starts its recording: %d, want 404", name, code)
		}
	}
	if got := egress.Started(room); len(got) != 0 {
		t.Fatalf("a refused start recorded the call: %d recordings", len(got))
	}
	if code := record(sam, true); code != http.StatusOK {
		t.Fatalf("its organiser starts its recording: %d, want 200", code)
	}
	started := egress.Started(room)
	if len(started) != 1 {
		t.Fatalf("recordings of the event's call: %d, want 1", len(started))
	}
	id := started[0].EgressId
	var node struct {
		Q []struct {
			Event string `json:"recording_event"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(recording_egress_id, "`+id+`")) { recording_event } }`), &node); err != nil {
		t.Fatal(err)
	}
	if len(node.Q) != 1 || node.Q[0].Event != e.ID {
		t.Fatalf("the recording's node = %+v, want one filed under the event %s", node.Q, e.ID)
	}
	if code := record(sam, false); code != http.StatusOK {
		t.Fatalf("its organiser stops its recording: %d, want 200", code)
	}
	lkWebhook(t, lk, &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: egress.End(t, id)})
	// Stopped and ended, nothing records the call: a second stop (another tab,
	// a view that is behind) is the asker's state, answered as on every route.
	again := call(t, callsController.SetMeetingRecording, http.MethodPost, "/calls/meetings/"+meeting+"/recording", sam, `{"on":false}`,
		map[string]string{"meeting_id": meeting})
	var said struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(again.Body.Bytes(), &said)
	if again.Code != http.StatusBadRequest || said.Msg != httpx.NothingRecording {
		t.Errorf("its organiser stops it again with nothing recording: %d %q, want 400 %q", again.Code, said.Msg, httpx.NothingRecording)
	}

	type artefact struct {
		Key      string `json:"key"`
		Recorded bool   `json:"recorded"`
	}
	code, body = v2(t, eventController.GetCalendarEventCallArtefacts, http.MethodGet, "/calendar/events/"+e.ID+"/call/artefacts", maya, "", e.ID)
	var listed []artefact
	_ = json.Unmarshal(body["data"], &listed)
	if code != http.StatusOK || !slices.Contains(listed, artefact{Key: id, Recorded: true}) {
		t.Errorf("the event's Recordings & notes: %d %+v, want the recording %s, recorded", code, listed, id)
	}
	play := func(who *userModels.UserInfo, key string) int {
		return call(t, eventController.GetCalendarEventCallRecording, http.MethodGet, "/calendar/events/"+e.ID+"/call/artefacts/"+key+"/recording",
			who, "", map[string]string{"eventId": e.ID, "key": key}).Code
	}
	for name, who := range map[string]*userModels.UserInfo{"its organiser": sam, "the member it invites": maya} {
		if code := play(who, id); code != http.StatusOK {
			t.Errorf("%s plays the event's recording: %d, want 200", name, code)
		}
	}
	if code := play(lee, id); code != http.StatusNotFound {
		t.Errorf("a member the event doesn't invite plays its recording: %d, want 404", code)
	}
	if code := play(sam, "EG_none"); code != http.StatusNotFound {
		t.Errorf("a recording the event has no such: %d, want 404", code)
	}

	// Whoever may change the event deletes the recording, and brings it back
	// (its Undo): the member it invites, who may only read it, is refused, and
	// a member it doesn't invite finds no such event. The delete is soft: the
	// list and the player stop showing it at once, and its node stays, stamped.
	change := func(who *userModels.UserInfo, restore bool) int {
		method, path, handler := http.MethodDelete, "/calendar/events/"+e.ID+"/call/artefacts/"+id, eventController.DeleteCalendarEventCallArtefact
		if restore {
			method, path, handler = http.MethodPost, path+"/restore", eventController.RestoreCalendarEventCallArtefact
		}
		return call(t, handler, method, path, who, "", map[string]string{"eventId": e.ID, "key": id}).Code
	}
	deletedAt := func() string {
		var kept struct {
			Q []struct {
				DeletedAt string `json:"recording_deleted_at"`
			} `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(recording_egress_id, "`+id+`")) { recording_deleted_at } }`), &kept); err != nil || len(kept.Q) != 1 {
			t.Fatalf("the recording's node: %+v, %v; want it kept", kept.Q, err)
		}
		return kept.Q[0].DeletedAt
	}
	listedNow := func() bool {
		code, body := v2(t, eventController.GetCalendarEventCallArtefacts, http.MethodGet, "/calendar/events/"+e.ID+"/call/artefacts", maya, "", e.ID)
		var now []artefact
		_ = json.Unmarshal(body["data"], &now)
		if code != http.StatusOK {
			t.Fatalf("the event's Recordings & notes: %d", code)
		}
		return slices.ContainsFunc(now, func(a artefact) bool { return a.Key == id })
	}
	if code := change(maya, false); code != http.StatusForbidden {
		t.Errorf("the member it invites, who may not change the event, deletes its recording: %d, want 403", code)
	}
	if code := change(lee, false); code != http.StatusNotFound {
		t.Errorf("a member it doesn't invite deletes its recording: %d, want 404", code)
	}
	if !listedNow() || play(maya, id) != http.StatusOK {
		t.Fatal("a refused delete hid the recording")
	}
	if code := change(sam, false); code != http.StatusOK {
		t.Fatalf("its organiser deletes its recording: %d, want 200", code)
	}
	if listedNow() {
		t.Error("the deleted recording is still in the event's Recordings & notes")
	}
	if code := play(maya, id); code != http.StatusNotFound {
		t.Errorf("the deleted recording plays: %d, want 404", code)
	}
	if at := deletedAt(); at < "1971" {
		t.Errorf("the deleted recording's node says deleted at %q, want the time it was deleted", at)
	}
	if code := change(sam, false); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code := change(maya, true); code != http.StatusForbidden {
		t.Errorf("the member it invites brings it back: %d, want 403", code)
	}
	if code := change(sam, true); code != http.StatusOK {
		t.Fatalf("its organiser's Undo: %d, want 200", code)
	}
	if !listedNow() || play(maya, id) != http.StatusOK || deletedAt() >= "1971" {
		t.Error("the recording brought back isn't listed and playable again")
	}
	if code := change(sam, true); code != http.StatusNotFound {
		t.Errorf("bringing back one that's there: %d, want 404", code)
	}
}
