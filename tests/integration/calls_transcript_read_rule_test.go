//go:build integration
// +build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	lksdk "github.com/livekit/server-sdk-go/v2"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/akashc777/OneCamp/tests/livekitfake"
)

// Every transcript read follows the one read rule (V-860): while a call goes
// on in a conversation, what a recording of it kept is for the people
// connected to it, through each conversation's own paged read and its
// ?format= export alike. Maya is a member of each and connected to none; once
// no call goes on, she reads them all again.
func TestEveryTranscriptReadFollowsTheOneReadRule(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "read-rule-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")

	// Each recording stopped a few minutes ago, inside a call that goes on.
	began, stopped := time.Now().Add(-10*time.Minute).UTC(), time.Now().Add(-5*time.Minute).UTC()
	rec := func(key, words string) map[string]any {
		return map[string]any{"uid": "_:" + strings.ToLower(key), "dgraph.type": "Recording", "recording_egress_id": key,
			"recording_stared_at": began.Format(time.RFC3339), "recording_ended_at": stopped.Format(time.RFC3339),
			"recording_obj_key": "recordings/" + key + ".mp4",
			"recording_transcript": []map[string]any{{"dgraph.type": "Transcript", "transcript_text": words,
				"transcript_timestamp": began.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": sam.UserDgraphInfo.Uid}}}}
	}
	people := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}
	channel := uuid.NewString()
	dm := helpers.GetGroupingId(sam.UserDgraphInfo.Uuid, maya.UserDgraphInfo.Uuid)
	group := strings.ReplaceAll(uuid.NewString(), "-", "")
	dg.Mutate(t, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch",
		"ch_members": people, "ch_recording": []map[string]any{rec("EG_live_ch", "channel words")}})
	dg.Mutate(t, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": dm,
		"dm_participants": people, "dm_recording": []map[string]any{rec("EG_live_dm", "direct words")}})
	dg.Mutate(t, map[string]any{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group,
		"dm_participants": people, "dm_recording": []map[string]any{rec("EG_live_grp", "group words")}})

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
	evRec := rec("EG_live_ev", "event words")
	evRec["recording_event"] = event
	dg.Mutate(t, evRec)

	type read struct {
		name, path, words string
		h                 http.HandlerFunc
		params            map[string]string
	}
	reads := []read{
		{"channel", "/channel/getRecordingTranscript/" + channel + "/EG_live_ch?pageIndex=0&pageSize=20", "channel words",
			channelController.GetChannelRecordingTranscript, map[string]string{"channel_uuid": channel, "egress_id": "EG_live_ch"}},
		{"direct", "/chat/getRecordingTranscript/" + sam.UserDgraphInfo.Uuid + "/EG_live_dm?pageIndex=0&pageSize=20", "direct words",
			chatController.GetChatRecordingTranscript, map[string]string{"chat_uuid": sam.UserDgraphInfo.Uuid, "egress_id": "EG_live_dm"}},
		{"group", "/chat/grp/getRecordingTranscript/" + group + "/EG_live_grp?pageIndex=0&pageSize=20", "group words",
			chatController.GetGrpChatRecordingTranscript, map[string]string{"grp_id": group, "egress_id": "EG_live_grp"}},
		{"event", "/calendar/events/" + event + "/call/artefacts/EG_live_ev/transcript?pageIndex=0&pageSize=20", "event words",
			eventController.GetCalendarEventCallTranscript, map[string]string{"eventId": event, "key": "EG_live_ev"}},
	}
	// Each read is made paged and as its ?format= export file alike.
	exports := make([]read, 0, len(reads))
	for _, r := range reads {
		r.name, r.path = r.name+"&format=txt", r.path+"&format=txt"
		exports = append(exports, r)
	}
	reads = append(reads, exports...)
	each := func(f func(r read, code int, body string)) {
		for _, r := range reads {
			w := call(t, r.h, http.MethodGet, r.path, maya, "", r.params)
			f(r, w.Code, w.Body.String())
		}
	}
	// Before any call goes on, Maya reads each one.
	each(func(r read, code int, body string) {
		if code != http.StatusOK || !strings.Contains(body, r.words) {
			t.Fatalf("%s with no call going on: %d %s", r.name, code, body)
		}
	})

	// A call goes on in each conversation, begun before the recordings were.
	open := map[string]string{"channel": channel, "dm": dm, "group": group, "meeting": keys.Room}
	calls := map[string]uuid.UUID{}
	for kind, room := range open {
		kindOf := "huddle"
		if kind == "meeting" {
			kindOf = "meeting"
		}
		calls[kind] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, $3, $4, $2, $5)`,
			calls[kind], room, kindOf, kind, began.Add(-10*time.Minute)); err != nil {
			t.Fatalf("open the %s call: %v", kind, err)
		}
	}
	// The channel's call has a breakout room open too.
	breakout := callStateBusiness.BreakoutRoom(channel, 1)
	if _, made, err := callStateModel.OpenSession(ctx, callStateModel.Session{ID: uuid.New(), Room: breakout, Kind: callStateModel.KindBreakout,
		ParentSessionID: &[]uuid.UUID{calls["channel"]}[0], ResourceKind: callStateModel.ResourceChannel, ResourceID: channel,
		StartedAt: began.Add(-5 * time.Minute)}); err != nil || !made {
		t.Fatalf("open the channel call's breakout room: %v %v", made, err)
	}
	each(func(r read, code int, body string) {
		if code == http.StatusOK || strings.Contains(body, r.words) {
			t.Errorf("%s while a call goes on, not connected: %d", r.name, code)
		}
	})

	// Connected to the call going on, Maya reads each one: what her own call
	// recorded is hers to read back.
	lk := livekitfake.Serve(t, channel, "RM_rule_ch", maya.UserDgraphInfo.Uid)
	for room, sid := range map[string]string{dm: "RM_rule_dm", group: "RM_rule_grp", keys.Room: "RM_rule_ev"} {
		lk.Room(room, sid, maya.UserDgraphInfo.Uid)
	}
	each(func(r read, code int, body string) {
		if code != http.StatusOK || !strings.Contains(body, r.words) {
			t.Errorf("%s while a call goes on, connected to it: %d %s", r.name, code, body)
		}
	})
	if !liveKitBusiness.NewestTranscriptOpenTo(ctx, channel, maya.UserDgraphInfo.Uid) {
		t.Errorf("the channel's newest transcript, connected to its call: no")
	}
	// Connected only to the channel call's breakout room, she reads it too.
	lk.Room(channel, "RM_rule_ch")
	lk.Room(breakout, "RM_rule_bo", maya.UserDgraphInfo.Uid)
	each(func(r read, code int, body string) {
		if code != http.StatusOK || !strings.Contains(body, r.words) {
			t.Errorf("%s while a call goes on, the channel's connected only to its breakout room: %d %s", r.name, code, body)
		}
	})

	// Unsure is no. With LiveKit not answering, nobody is taken to be
	// connected, and no read is open while the calls go on. The connected
	// service is put back when this step ends, for the steps after it.
	func() {
		connected := livekitInit.LiveKitService
		defer func() { livekitInit.LiveKitService = connected }()
		down := httptest.NewServer(http.NotFoundHandler())
		down.Close()
		livekitInit.LiveKitService = livekitInit.LiveKitServiceStruct{LiveKitClient: lksdk.NewRoomServiceClient(down.URL, "test-key", "test-secret-long-enough-to-sign-with")}
		each(func(r read, code int, body string) {
			if code == http.StatusOK || strings.Contains(body, r.words) {
				t.Errorf("%s while a call goes on, LiveKit not answering: %d", r.name, code)
			}
		})
		if liveKitBusiness.NewestTranscriptOpenTo(ctx, channel, maya.UserDgraphInfo.Uid) {
			t.Errorf("the channel's newest transcript, LiveKit not answering: yes")
		}
	}()
	// Nor is any open, to her connected, while the calls' state can't be read.
	restore := func() {
		if _, err := env.PG.Exec(`ALTER TABLE IF EXISTS call_sessions_unread RENAME TO call_sessions`); err != nil {
			t.Errorf("give the calls' state back: %v", err)
		}
	}
	t.Cleanup(restore)
	if _, err := env.PG.Exec(`ALTER TABLE call_sessions RENAME TO call_sessions_unread`); err != nil {
		t.Fatal(err)
	}
	each(func(r read, code int, body string) {
		if code == http.StatusOK || strings.Contains(body, r.words) {
			t.Errorf("%s while a call goes on, its state unreadable: %d", r.name, code)
		}
	})
	if liveKitBusiness.TranscriptOpenTo(ctx, channel, maya.UserDgraphInfo.Uid, "EG_live_ch", &began) ||
		liveKitBusiness.NewestTranscriptOpenTo(ctx, channel, maya.UserDgraphInfo.Uid) {
		t.Errorf("the calls' state unreadable: the rule said yes")
	}
	restore()

	// The calls end: the transcripts are their conversations' again.
	if _, err := env.PG.Exec(`UPDATE call_sessions SET ended_at = now() WHERE ended_at IS NULL AND room IN ($1, $2, $3, $4, $5)`,
		channel, dm, group, keys.Room, breakout); err != nil {
		t.Fatal(err)
	}
	each(func(r read, code int, body string) {
		if code != http.StatusOK || !strings.Contains(body, r.words) {
			t.Errorf("%s once the call ended: %d %s", r.name, code, body)
		}
	})
}
