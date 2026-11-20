//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/livekitfake"
)

// Every recording link follows the one read rule its transcript does: while
// a call goes on in a conversation, a recording that call made is for the
// people connected to it. Maya is a member of each conversation and invited
// to the event; Lee is in none of them. In each place one recording stopped a
// few minutes ago, inside a call that goes on, and an earlier call's an hour
// ago, which follows the standing rules while the new call goes on.
func TestEveryRecordingLinkFollowsTheOneReadRule(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "read-rule-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupMinio(t, "recording-read-rule-test")
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")

	began, stopped := time.Now().Add(-10*time.Minute).UTC(), time.Now().Add(-5*time.Minute).UTC()
	lastBegan, lastStopped := time.Now().Add(-time.Hour).UTC(), time.Now().Add(-55*time.Minute).UTC()
	rec := func(key, place, of string, from, to time.Time) map[string]any {
		r := map[string]any{"uid": "_:" + strings.ToLower(key), "dgraph.type": "Recording", "recording_egress_id": key,
			"recording_stared_at": from.Format(time.RFC3339), "recording_ended_at": to.Format(time.RFC3339),
			"recording_obj_key": "recordings/" + key + ".mp4", "recording_status": "ready", "recording_duration": 300}
		if place != "" {
			r[place] = map[string]any{"uid": of}
		}
		return r
	}
	people := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}
	channel := uuid.NewString()
	dm := helpers.GetGroupingId(sam.UserDgraphInfo.Uuid, maya.UserDgraphInfo.Uuid)
	group := strings.ReplaceAll(uuid.NewString(), "-", "")
	both := func(place, of, at string) []map[string]any {
		return []map[string]any{rec("EG_link_"+at, place, of, began, stopped), rec("EG_last_"+at, place, of, lastBegan, lastStopped)}
	}
	dg.Mutate(t, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch",
		"ch_members": people, "ch_recording": both("recording_channel", "_:ch", "ch")})
	dg.Mutate(t, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": dm,
		"dm_participants": people, "dm_recording": both("recording_dm", "_:dm", "dm")})
	dg.Mutate(t, map[string]any{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group,
		"dm_participants": people, "dm_recording": both("recording_dm", "_:grp", "grp")})

	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Review","start":{"at":"`+day.Add(9*time.Hour).Format(time.RFC3339)+`","zone":"Europe/London"},"end":{"at":"`+
			day.Add(10*time.Hour).Format(time.RFC3339)+`"},"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create the event: %d %v", code, body)
	}
	event := dataEvent(t, body).ID
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+event+"/call", sam, "", event); code != http.StatusOK {
		t.Fatalf("the event's call: %d %v", code, body)
	}
	keys, err := calendarBusiness.Events.CallArtefactKeys(ctx, maya, event)
	if err != nil || keys.Room == "" {
		t.Fatalf("the event's call room: %q %v", keys.Room, err)
	}
	for _, evRec := range both("", "", "ev") {
		evRec["recording_event"] = event
		dg.Mutate(t, evRec)
	}

	type link struct {
		name, path string
		h          http.HandlerFunc
		params     map[string]string
	}
	// linksTo is the four links to the recordings named EG_<of>_<place>.
	linksTo := func(of string) []link {
		key := func(at string) string { return "EG_" + of + "_" + at }
		return []link{
			{"channel", "/channel/getRecordingURL/" + channel + "/" + key("ch"), userController.GetChannelRecordingURL,
				map[string]string{"channel_uuid": channel, "egress_id": key("ch")}},
			{"direct", "/chat/getRecordingURL/" + sam.UserDgraphInfo.Uuid + "/" + key("dm"), userController.GetChatRecordingURL,
				map[string]string{"chat_uuid": sam.UserDgraphInfo.Uuid, "egress_id": key("dm")}},
			{"group", "/chat/grp/getRecordingURL/" + group + "/" + key("grp"), userController.GetGrpChatRecordingURL,
				map[string]string{"grp_id": group, "egress_id": key("grp")}},
			{"event", "/calendar/events/" + event + "/call/artefacts/" + key("ev") + "/recording", eventController.GetCalendarEventCallRecording,
				map[string]string{"eventId": event, "key": key("ev")}},
		}
	}
	links, earlier := linksTo("link"), linksTo("last")
	ask := func(l link, who *userModels.UserInfo) (int, map[string]any) {
		w := call(t, l.h, http.MethodGet, l.path, who, "", l.params)
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	plays := func(when string) {
		t.Helper()
		for _, l := range append(links, earlier...) {
			if code, got := ask(l, maya); code != http.StatusOK || got["url"] == nil || got["status"] != "ready" {
				t.Errorf("%s %s: %d %v, want a link", l.name, when, code, got["msg"])
			}
		}
	}
	strangerRefused := func(when string) {
		t.Helper()
		for _, l := range append(links, earlier...) {
			if code, got := ask(l, lee); code != http.StatusNotFound || got["url"] != nil {
				t.Errorf("%s, Lee in none of them, %s: %d, want 404 and no link", l.name, when, code)
			}
		}
	}

	// With no call going on, Maya plays each one; Lee is answered as if there
	// were none (V-1126).
	plays("with no call going on")
	strangerRefused("with no call going on")

	// A call goes on in each conversation, begun before the recordings were.
	open := map[string]string{"channel": channel, "dm": dm, "group": group, "meeting": keys.Room}
	for kind, room := range open {
		kindOf := "huddle"
		if kind == "meeting" {
			kindOf = "meeting"
		}
		if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, $3, $4, $2, $5)`,
			uuid.New(), room, kindOf, kind, began.Add(-10*time.Minute)); err != nil {
			t.Fatalf("open the %s call: %v", kind, err)
		}
	}
	// Not connected to it, Maya is answered as if there were no recording.
	for _, l := range links {
		if code, got := ask(l, maya); code != http.StatusNotFound || got["msg"] != "Recording not found" || got["url"] != nil {
			t.Errorf("%s while its call goes on, not connected: %d %v, want 404 Recording not found", l.name, code, got["msg"])
		}
	}
	// An earlier call's recording, begun before the call going on, plays for
	// her all the same.
	for _, l := range earlier {
		if code, got := ask(l, maya); code != http.StatusOK || got["msg"] != "Got the url!" || got["url"] == nil {
			t.Errorf("%s, an earlier call's, while a new call goes on, not connected: %d %v, want a link", l.name, code, got["msg"])
		}
	}
	strangerRefused("while a call goes on")

	// Connected to the call going on, she plays each one.
	lk := livekitfake.Serve(t, channel, "RM_link_ch", maya.UserDgraphInfo.Uid)
	for room, sid := range map[string]string{dm: "RM_link_dm", group: "RM_link_grp", keys.Room: "RM_link_ev"} {
		lk.Room(room, sid, maya.UserDgraphInfo.Uid)
	}
	plays("while its call goes on, connected to it")
	strangerRefused("while a call goes on, Maya connected")

	// The calls end: the recordings are their conversations' again.
	for room, sid := range map[string]string{channel: "RM_link_ch", dm: "RM_link_dm", group: "RM_link_grp", keys.Room: "RM_link_ev"} {
		lk.Room(room, sid)
	}
	if _, err := env.PG.Exec(`UPDATE call_sessions SET ended_at = now() WHERE ended_at IS NULL AND room IN ($1, $2, $3, $4)`,
		channel, dm, group, keys.Room); err != nil {
		t.Fatal(err)
	}
	plays("once its call ended")
	strangerRefused("once the calls ended")
}
