//go:build integration

package integration_test

// A repeating meeting's call belongs to the one instance whose time it
// happened in (V-1061): its recording, transcript, recap and action items
// show on that instance alone, a future instance with no call of its own
// draws nothing, and the series' own id still lists every call it held.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/akashc777/OneCamp/business/CallRoom"  // hands the calendar its event calls
	_ "github.com/akashc777/OneCamp/business/CallState" // reads what an event's call made
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestASeriesCallShowsOnTheInstanceItHappenedIn(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "series-call-instance-secret")
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

	// A daily Standup of three: yesterday, today and tomorrow at 09:00 UTC,
	// so the third is always still to come.
	first := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1).Add(9 * time.Hour)
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Standup","start":{"at":"`+first.Format(time.RFC3339)+`","zone":"UTC"},"end":{"at":"`+first.Add(30*time.Minute).Format(time.RFC3339)+`"},`+
			`"recurrence":{"rule":"FREQ=DAILY;COUNT=3"}}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	series := dataEvent(t, body).ID
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+series+"/call", sam, "", series); code != http.StatusOK {
		t.Fatalf("its call: %d %v", code, body)
	}
	ref := func(k int) string {
		return series + "_" + first.AddDate(0, 0, k-1).Format("20060102T150405Z")
	}

	// One call, opened two minutes before the second Standup, filed under the
	// series as every instance's call files: its transcript, recap and items.
	key := helpers.CallSessionKeyFor(uuid.NewString())
	began := first.AddDate(0, 0, 1).Add(-2 * time.Minute)
	dg.Mutate(t, map[string]any{"uid": "_:rec", "dgraph.type": "Recording", "recording_egress_id": key, "recording_event": series,
		"recording_transcript_only": true, "recording_stared_at": began.Format(time.RFC3339),
		"recording_ended_at": began.Add(25 * time.Minute).Format(time.RFC3339),
		"recording_recap":    "We shipped the board.", "recording_action_items": `[{"text":"Send the notes"}]`,
		"recording_transcript": []map[string]any{{"dgraph.type": "Transcript", "transcript_text": "Morning all",
			"transcript_timestamp": began.Add(time.Minute).UnixMilli(), "transcript_from": map[string]any{"uid": sam.UserDgraphInfo.Uid}}}})

	keyed := func(h http.HandlerFunc, method, path, id, body string) (int, string) {
		rec := call(t, h, method, path, sam, body, map[string]string{"eventId": id, "key": key, "index": "0"})
		return rec.Code, rec.Body.String()
	}
	listed := func(id string) []string {
		code, body := v2(t, eventController.GetCalendarEventCallArtefacts, http.MethodGet, "/calendar/events/"+id+"/call/artefacts", sam, "", id)
		var got []struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(body["data"], &got); code != http.StatusOK || err != nil {
			t.Fatalf("%s's artefacts: %d %v", id, code, body)
		}
		var keys []string
		for _, a := range got {
			keys = append(keys, a.Key)
		}
		return keys
	}
	shows := func(id string) bool {
		keys := listed(id)
		return len(keys) == 1 && keys[0] == key
	}

	if !shows(series) {
		t.Errorf("the series' own id lists %v, want its one call %s", listed(series), key)
	}
	if !shows(ref(2)) {
		t.Errorf("the second Standup lists %v, want the call it held %s", listed(ref(2)), key)
	}
	for _, k := range []int{1, 3} {
		if got := listed(ref(k)); len(got) != 0 {
			t.Errorf("Standup %d lists %v, want nothing: the call was the second's", k, got)
		}
	}

	path := func(id, rest string) string { return "/calendar/events/" + id + "/call/artefacts/" + key + rest }
	for _, k := range []int{1, 3} {
		id := ref(k)
		if code, _ := keyed(eventController.GetCalendarEventCallRecording, http.MethodGet, path(id, "/recording"), id, ""); code != http.StatusNotFound {
			t.Errorf("Standup %d plays the second's recording: %d, want 404", k, code)
		}
		if code, _ := keyed(eventController.GetCalendarEventCallTranscript, http.MethodGet, path(id, "/transcript"), id, ""); code != http.StatusNotFound {
			t.Errorf("Standup %d reads the second's transcript: %d, want 404", k, code)
		}
		if code, body := keyed(eventController.MakeCalendarEventCallActionTask, http.MethodPost, path(id, "/action-items/0/task"), id,
			`{"project_uuid":"`+uuid.NewString()+`"}`); code != http.StatusNotFound || !strings.Contains(body, "no such action item") {
			t.Errorf("Standup %d makes a task of the second's item: %d %s, want 404 no such action item", k, code, body)
		}
		if code, _ := keyed(eventController.DeleteCalendarEventCallArtefact, http.MethodDelete, path(id, ""), id, ""); code != http.StatusNotFound {
			t.Errorf("Standup %d deletes the second's call: %d, want 404", k, code)
		}
	}
	if !shows(ref(2)) {
		t.Fatalf("a refused delete took the call off the second Standup: %v", listed(ref(2)))
	}

	// The second Standup reads its own call. (On the AI edition its item also
	// reaches the project check; this edition makes no action items' tasks.)
	if code, body := keyed(eventController.GetCalendarEventCallTranscript, http.MethodGet, path(ref(2), "/transcript"), ref(2), ""); code != http.StatusOK || !strings.Contains(body, "Morning all") {
		t.Errorf("the second Standup reads its transcript: %d %s", code, body)
	}

	// Deleted by the instance it belongs to; brought back only by it.
	if code, body := keyed(eventController.DeleteCalendarEventCallArtefact, http.MethodDelete, path(ref(2), ""), ref(2), ""); code >= 300 {
		t.Fatalf("the second Standup deletes its call: %d %s", code, body)
	}
	for _, k := range []int{1, 3} {
		id := ref(k)
		if code, _ := keyed(eventController.RestoreCalendarEventCallArtefact, http.MethodPost, path(id, "/restore"), id, ""); code != http.StatusNotFound {
			t.Errorf("Standup %d restores the second's call: %d, want 404", k, code)
		}
	}
	if code, body := keyed(eventController.RestoreCalendarEventCallArtefact, http.MethodPost, path(ref(2), "/restore"), ref(2), ""); code >= 300 {
		t.Fatalf("the second Standup restores its call: %d %s", code, body)
	}
	if !shows(ref(2)) || !shows(series) {
		t.Errorf("after the restore: second %v, series %v, want the call on both", listed(ref(2)), listed(series))
	}

	// A call with no start (a transcript-only node the boot repair ended, which
	// writes none) is placed by its end: ten minutes into the first Standup.
	bare := helpers.CallSessionKeyFor(uuid.NewString())
	dg.Mutate(t, map[string]any{"uid": "_:bare", "dgraph.type": "Recording", "recording_egress_id": bare, "recording_event": series,
		"recording_transcript_only": true, "recording_ended_at": first.Add(10 * time.Minute).Format(time.RFC3339),
		"recording_transcript": []map[string]any{{"dgraph.type": "Transcript", "transcript_text": "Cut off",
			"transcript_timestamp": first.UnixMilli(), "transcript_from": map[string]any{"uid": sam.UserDgraphInfo.Uid}}}})
	if got := listed(ref(1)); !slices.Equal(got, []string{bare}) {
		t.Errorf("the first Standup lists %v, want the call with no start that ended in it %s", got, bare)
	}
	for _, k := range []int{2, 3} {
		if got := listed(ref(k)); slices.Contains(got, bare) {
			t.Errorf("Standup %d lists the first's call with no start: %v", k, got)
		}
	}
	if got := listed(series); !slices.Contains(got, bare) || !slices.Contains(got, key) {
		t.Errorf("the series' own id lists %v, want both its calls", got)
	}
	transcript := func(k int) (int, string) {
		id := ref(k)
		rec := call(t, eventController.GetCalendarEventCallTranscript, http.MethodGet, "/calendar/events/"+id+"/call/artefacts/"+bare+"/transcript", sam, "",
			map[string]string{"eventId": id, "key": bare, "index": "0"})
		return rec.Code, rec.Body.String()
	}
	if code, body := transcript(1); code != http.StatusOK || !strings.Contains(body, "Cut off") {
		t.Errorf("the first Standup reads the transcript of its call with no start: %d %s", code, body)
	}
	if code, _ := transcript(2); code != http.StatusNotFound {
		t.Errorf("the second Standup reads the first's call with no start: %d, want 404", code)
	}
}
