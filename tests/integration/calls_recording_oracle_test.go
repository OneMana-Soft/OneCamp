//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Nobody outside a place learns whether a recording is there (V-1126): on
// every recording route, a recording the asker can't see (not in its channel
// or conversation, or asked for through another place's route) is answered
// exactly as one there isn't, the same status and the same body. Sam and Maya
// are in each place, Sam moderates the channel, both are on the event; Lee is
// in none of them. A member who may watch a recording but not change it keeps
// the delete's and the restore's 403 sentence.
func TestNobodyOutsideAPlaceLearnsWhetherARecordingIsThere(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "recording-oracle-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupMinio(t, "recording-oracle-test") // a reader's link is signed
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")

	// Recordings an hour old, with no call going on; the ones named gone were
	// deleted (their Undo is the restore). Ids are random, as egress ids are.
	n := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	key := func(at string) string { return "EG_" + n + "_" + at }
	began, stopped := time.Now().Add(-time.Hour).UTC(), time.Now().Add(-55*time.Minute).UTC()
	rec := func(at, place, of string) map[string]any {
		r := map[string]any{"uid": "_:" + at, "dgraph.type": "Recording", "recording_egress_id": key(at),
			"recording_stared_at": began.Format(time.RFC3339), "recording_ended_at": stopped.Format(time.RFC3339),
			"recording_obj_key": "recordings/" + key(at) + ".mp4", "recording_status": "ready", "recording_duration": 300,
			"recording_transcript": []map[string]any{{"uid": "_:" + at + "line", "dgraph.type": "Transcript", "transcript_text": "words",
				"transcript_timestamp": began.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": sam.UserDgraphInfo.Uid}}}}
		if strings.HasSuffix(at, "gone") {
			r["recording_deleted_at"] = stopped.Add(time.Minute).Format(time.RFC3339)
		}
		if place != "" {
			r[place] = map[string]any{"uid": of}
		}
		return r
	}
	people := []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}}
	launch, ops := uuid.NewString(), uuid.NewString()
	group := strings.ReplaceAll(uuid.NewString(), "-", "")
	launchUids := dg.Mutate(t, map[string]any{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch",
		"ch_members": people, "ch_moderators": []map[string]any{{"uid": sam.UserDgraphInfo.Uid}},
		"ch_recording": []map[string]any{rec("ch", "recording_channel", "_:ch"), rec("chgone", "recording_channel", "_:ch")}})
	dg.Mutate(t, map[string]any{"uid": "_:ops", "dgraph.type": "Channel", "ch_uuid": ops, "ch_name": "ops",
		"ch_members": people, "ch_recording": []map[string]any{rec("ops", "recording_channel", "_:ops")}})
	dg.Mutate(t, map[string]any{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": helpers.GetGroupingId(sam.UserDgraphInfo.Uuid, maya.UserDgraphInfo.Uuid),
		"dm_participants": people, "dm_recording": []map[string]any{rec("dm", "recording_dm", "_:dm"), rec("dmgone", "recording_dm", "_:dm")}})
	dg.Mutate(t, map[string]any{"uid": "_:grp", "dgraph.type": "Dm", "dm_grouping_id": group,
		"dm_participants": people, "dm_recording": []map[string]any{rec("grp", "recording_dm", "_:grp"), rec("grpgone", "recording_dm", "_:grp")}})

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
	ev := rec("ev", "", "")
	ev["recording_event"] = event
	dg.Mutate(t, ev)

	// route is one recording route, asking for one recording by its id.
	type route struct {
		name, method, path string
		h                  http.HandlerFunc
		params             map[string]string
		body               string
	}
	routeTo := func(at string) []route {
		k, place := key(at), launch
		if at == "missing" {
			k = "EG_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + "_none"
			place = uuid.NewString()
		}
		chat := sam.UserDgraphInfo.Uuid
		return []route{
			{"channel list", http.MethodGet, "/ch/getRecordingList/" + place + "?pageIndex=0&pageSize=20", channelController.GetAllChannelRecordingList,
				map[string]string{"channel_uuid": place}, ""},
			{"channel link", http.MethodGet, "/channel/getRecordingURL/" + launch + "/" + k, userController.GetChannelRecordingURL,
				map[string]string{"channel_uuid": launch, "egress_id": k}, ""},
			{"direct link", http.MethodGet, "/chat/getRecordingURL/" + chat + "/" + k, userController.GetChatRecordingURL,
				map[string]string{"chat_uuid": chat, "egress_id": k}, ""},
			{"group link", http.MethodGet, "/chat/grp/getRecordingURL/" + group + "/" + k, userController.GetGrpChatRecordingURL,
				map[string]string{"grp_id": group, "egress_id": k}, ""},
			{"channel delete", http.MethodPost, "/channel/deleteRecording/" + k, recordingController.HandleDeleteChannelRecording,
				map[string]string{"egressId": k}, ""},
			{"chat delete", http.MethodPost, "/chat/deleteRecording/" + k, recordingController.HandleDeleteChatRecording,
				map[string]string{"egressId": k}, ""},
			{"channel restore", http.MethodPost, "/channel/restoreRecording/" + k, recordingController.HandleRestoreChannelRecording,
				map[string]string{"egressId": k}, ""},
			{"chat restore", http.MethodPost, "/chat/restoreRecording/" + k, recordingController.HandleRestoreChatRecording,
				map[string]string{"egressId": k}, ""},
			{"channel transcript", http.MethodGet, "/channel/getRecordingTranscript/" + launch + "/" + k + "?pageIndex=0&pageSize=20",
				channelController.GetChannelRecordingTranscript, map[string]string{"channel_uuid": launch, "egress_id": k}, ""},
			{"direct transcript", http.MethodGet, "/chat/getRecordingTranscript/" + chat + "/" + k + "?pageIndex=0&pageSize=20",
				chatController.GetChatRecordingTranscript, map[string]string{"chat_uuid": chat, "egress_id": k}, ""},
			{"group transcript", http.MethodGet, "/chat/grp/getRecordingTranscript/" + group + "/" + k + "?pageIndex=0&pageSize=20",
				chatController.GetGrpChatRecordingTranscript, map[string]string{"grp_id": group, "egress_id": k}, ""},
			{"transcript export", http.MethodGet, "/calls/recordings/" + k + "/transcript?format=txt", recordingController.HandleExportTranscript,
				map[string]string{"key": k}, ""},
			{"transcript correction", http.MethodPatch, "/calls/recordings/" + k + "/transcript/lines/0x1", recordingController.HandleCorrectTranscriptLine,
				map[string]string{"key": k, "line_uid": "0x1"}, `{"text":"other words","was":"words"}`},
			{"event link", http.MethodGet, "/calendar/events/" + event + "/call/artefacts/" + k + "/recording", eventController.GetCalendarEventCallRecording,
				map[string]string{"eventId": event, "key": k}, ""},
			{"event transcript", http.MethodGet, "/calendar/events/" + event + "/call/artefacts/" + k + "/transcript?pageIndex=0&pageSize=20",
				eventController.GetCalendarEventCallTranscript, map[string]string{"eventId": event, "key": k}, ""},
		}
	}
	// keep is one answer someone inside launch gets today on one route.
	type keep struct {
		whoName string
		who     *userModels.UserInfo
		r       route
		want    int
	}
	ask := func(r route, who *userModels.UserInfo) (int, []byte) {
		w := call(t, r.h, r.method, r.path, who, r.body, r.params)
		return w.Code, w.Body.Bytes()
	}
	routeNamed := func(name, at string) route {
		t.Helper()
		for _, r := range routeTo(at) {
			if r.name == name {
				return r
			}
		}
		t.Fatalf("no route %q", name)
		return route{}
	}
	// sameAsMissing asks for the recording at, then for one there isn't, on
	// the route named, as who: the same answer, a 404 where wantNotFound.
	sameAsMissing := func(who *userModels.UserInfo, whoName, routeName, at string, wantNotFound bool) {
		t.Helper()
		code, got := ask(routeNamed(routeName, at), who)
		missingCode, missing := ask(routeNamed(routeName, "missing"), who)
		if code != missingCode || !bytes.Equal(got, missing) || (wantNotFound && code != http.StatusNotFound) {
			t.Errorf("%s, %s, a recording there (%s) against one there isn't: %d %s, want %d %s", routeName, whoName, at,
				code, strings.TrimSpace(string(got)), missingCode, strings.TrimSpace(string(missing)))
		}
	}

	// Lee, in none of the places: every recording route answers each one as
	// it answers an id there isn't.
	for _, c := range []struct {
		route, at    string
		wantNotFound bool
	}{
		{"channel link", "ch", true}, {"direct link", "dm", true}, {"group link", "grp", true},
		{"channel delete", "ch", true}, {"chat delete", "dm", true}, {"chat delete", "grp", true},
		{"channel restore", "chgone", true}, {"chat restore", "dmgone", true}, {"chat restore", "grpgone", true},
		{"channel transcript", "ch", false}, {"direct transcript", "dm", false}, {"group transcript", "grp", false},
		{"transcript export", "ch", true}, {"transcript correction", "ch", true},
		{"event link", "ev", false}, {"event transcript", "ev", false}, {"channel list", "ch", true},
	} {
		sameAsMissing(lee, "Lee, in none of them", c.route, c.at, c.wantNotFound)
	}
	// Lee as a workspace admin is still outside launch: every one of its
	// recording routes answers as for a recording, or a channel, there isn't,
	// its delete and Undo too, and nothing changes (V-1163, V-1166).
	leeAdmin := *lee
	leeAdmin.UserPostgresInfo.IsAdmin = true
	for _, c := range []struct {
		route, at    string
		wantNotFound bool
	}{
		{"channel list", "ch", true}, {"channel link", "ch", true}, {"channel transcript", "ch", false},
		{"channel delete", "ch", true}, {"channel restore", "chgone", true},
		{"transcript export", "ch", true}, {"transcript correction", "ch", true},
	} {
		sameAsMissing(&leeAdmin, "Lee, a workspace admin outside launch", c.route, c.at, c.wantNotFound)
	}
	// A channel there isn't answers launch's transcript route exactly as launch
	// does to Lee, and to Lee as a workspace admin: the same status, headers
	// and body, nothing of the read's error in it (V-1163).
	for _, who := range []struct {
		name string
		u    *userModels.UserInfo
	}{{"Lee, in none of them", lee}, {"Lee, a workspace admin outside launch", &leeAdmin}} {
		r, nowhere := routeNamed("channel transcript", "ch"), uuid.NewString()
		there := call(t, r.h, r.method, r.path, who.u, r.body, r.params)
		r.path, r.params = strings.Replace(r.path, launch, nowhere, 1), map[string]string{"channel_uuid": nowhere, "egress_id": key("ch")}
		missing := call(t, r.h, r.method, r.path, who.u, r.body, r.params)
		if there.Code != missing.Code || !reflect.DeepEqual(there.Header(), missing.Header()) || !bytes.Equal(there.Body.Bytes(), missing.Body.Bytes()) {
			t.Errorf("channel transcript, %s, launch against a channel there isn't: %d %v %s, want %d %v %s", who.name,
				there.Code, there.Header(), strings.TrimSpace(there.Body.String()), missing.Code, missing.Header(), strings.TrimSpace(missing.Body.String()))
		}
	}
	// Maya, in each place, asking through another place's route: launch's for
	// ops's recording, the group's for the DM's.
	sameAsMissing(maya, "Maya, through launch's route", "channel link", "ops", true)
	sameAsMissing(maya, "Maya, through the group's route", "group link", "dm", true)

	// Maya may watch launch's recordings, not change them: told so, in the
	// sentence a member gets.
	for _, c := range []struct{ route, at, msg string }{
		{"channel delete", "ch", "Only the channel's moderators can delete its recordings."},
		{"channel restore", "chgone", "Only the channel's moderators can restore its recordings."},
	} {
		r := routeNamed(c.route, c.at)
		if w := call(t, r.h, r.method, r.path, maya, r.body, r.params); w.Code != http.StatusForbidden || msgOf(w) != c.msg {
			t.Errorf("%s, Maya, a member: %d %q, want 403 %q", c.route, w.Code, msgOf(w), c.msg)
		}
	}

	// Inside launch nobody's answer narrows (V-1166 changes only who is
	// outside it): its member Maya, its moderator Sam, and Maya as a
	// workspace admin in it each get today's answer on every one of its
	// recording routes. The changes run in an order that leaves each recording
	// as it was: Sam and the admin each delete ch and bring it back, and
	// correct its line (Sam's) one way and back.
	mayaAdmin := *maya
	mayaAdmin.UserPostgresInfo.IsAdmin = true
	correction := func(was, text string) route {
		r, line := routeNamed("transcript correction", "ch"), launchUids["chline"]
		r.path = "/calls/recordings/" + key("ch") + "/transcript/lines/" + line
		r.params = map[string]string{"key": key("ch"), "line_uid": line}
		r.body = `{"text":"` + text + `","was":"` + was + `"}`
		return r
	}
	reads := func(whoName string, who *userModels.UserInfo) []keep {
		return []keep{{whoName, who, routeNamed("channel list", "ch"), http.StatusOK},
			{whoName, who, routeNamed("channel link", "ch"), http.StatusOK},
			{whoName, who, routeNamed("channel transcript", "ch"), http.StatusOK}}
	}
	keeps := append(reads("Maya, a member", maya),
		keep{"Maya, a member", maya, routeNamed("channel delete", "ch"), http.StatusForbidden},
		keep{"Maya, a member", maya, routeNamed("channel restore", "chgone"), http.StatusForbidden},
		keep{"Maya, a member", maya, correction("words", "other words"), http.StatusForbidden})
	keeps = append(append(keeps, reads("Sam, its moderator", sam)...),
		keep{"Sam, its moderator", sam, routeNamed("channel delete", "ch"), http.StatusOK},
		keep{"Sam, its moderator", sam, routeNamed("channel restore", "ch"), http.StatusOK},
		keep{"Sam, its moderator", sam, correction("words", "other words"), http.StatusOK})
	keeps = append(append(keeps, reads("Maya, a workspace admin in it", &mayaAdmin)...),
		keep{"Maya, a workspace admin in it", &mayaAdmin, routeNamed("channel delete", "ch"), http.StatusOK},
		keep{"Maya, a workspace admin in it", &mayaAdmin, routeNamed("channel restore", "ch"), http.StatusOK},
		keep{"Maya, a workspace admin in it", &mayaAdmin, correction("other words", "words"), http.StatusOK})
	for _, c := range keeps {
		if code, got := ask(c.r, c.who); code != c.want {
			t.Errorf("%s, %s: %d %s, want %d as before", c.r.name, c.whoName, code, strings.TrimSpace(string(got)), c.want)
		}
	}

	// Nothing Lee or Maya asked changed a recording: each is still there, in
	// the state it was, for whoever may change it.
	for _, c := range []struct {
		route, at string
		who       *userModels.UserInfo
	}{
		{"channel delete", "ch", sam}, {"channel restore", "chgone", sam},
		{"chat delete", "dm", maya}, {"chat delete", "grp", maya}, {"chat restore", "dmgone", maya}, {"chat restore", "grpgone", maya},
	} {
		if code, got := ask(routeNamed(c.route, c.at), c.who); code != http.StatusOK {
			t.Errorf("%s of %s by whoever may: %d %s, want 200", c.route, c.at, code, strings.TrimSpace(string(got)))
		}
	}
}
