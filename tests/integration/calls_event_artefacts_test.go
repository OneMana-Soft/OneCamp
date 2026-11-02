//go:build integration

package integration_test

// What an event's call makes lands on the event (meet-02): a call nobody
// recorded keeps its transcript on the node filed under its event, and the
// event's "Recordings & notes" lists it for whoever may read the event (the
// calendar's own rule): its organiser and the member it invites, never a
// member it doesn't invite. An instant meeting still keeps nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/akashc777/OneCamp/business/CallRoom"  // hands the calendar its event calls
	_ "github.com/akashc777/OneCamp/business/CallState" // hands business/LiveKit the event of a room's call
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestAnEventsCallKeepsItsTranscriptOnTheEvent(t *testing.T) {
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
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"email":"guest@example.org","name":"Guest"}]}`, "")
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

	// A call nobody recorded keeps its transcript, on a node filed under the event.
	key := helpers.CallSessionKeyFor(uuid.NewString())
	if err := liveKitBusiness.EnsureCallSessionNode(ctx, key, room); err != nil {
		t.Fatalf("an event's call keeps no transcript: %v", err)
	}
	if uid, linked, err := recordingDomain.FindCallSessionNode(ctx, key); err != nil || uid == "" || !linked {
		t.Fatalf("the call's transcript node = %q linked %v, %v; want one filed under the event", uid, linked, err)
	}
	liveKitBusiness.EndCallSession(ctx, key, time.Now().Add(-20*time.Minute))

	type artefact struct {
		Key       string     `json:"key"`
		StartedAt *time.Time `json:"started_at"`
		EndedAt   *time.Time `json:"ended_at"`
	}
	artefacts := func(who *userModels.UserInfo) (int, []artefact) {
		code, body := v2(t, eventController.GetCalendarEventCallArtefacts, http.MethodGet, "/calendar/events/"+e.ID+"/call/artefacts", who, "", e.ID)
		var out []artefact
		_ = json.Unmarshal(body["data"], &out)
		return code, out
	}
	for name, who := range map[string]*userModels.UserInfo{"its organiser": sam, "the member it invites": maya} {
		code, got := artefacts(who)
		if code != http.StatusOK || len(got) != 1 || got[0].Key != key || got[0].StartedAt == nil || got[0].EndedAt == nil {
			t.Errorf("%s reads the event's artefacts: %d %+v, want the one call %s, begun and ended", name, code, got, key)
		}
	}
	if code, got := artefacts(lee); code == http.StatusOK || len(got) != 0 {
		t.Errorf("a member the event doesn't invite read its artefacts: %d %+v", code, got)
	}

	// An instant meeting has no event to keep its words on: captions only.
	if err := liveKitBusiness.EnsureCallSessionNode(ctx, helpers.CallSessionKeyFor(uuid.NewString()), "meet-"+uuid.NewString()); !errors.Is(err, liveKitBusiness.ErrTranscriptNotKept) {
		t.Errorf("an instant meeting's call: %v, want ErrTranscriptNotKept", err)
	}
}

// A channel call's breakout room keeps its transcript where its call does: in
// the channel. The breakout's room name ("<channel>~b1") was once taken for a
// channel's uuid, and the transcript was filed under a NEW Channel node with
// that name as its uuid, which no list showed and nobody could reach.
func TestAChannelCallsBreakoutKeepsItsTranscriptInItsChannel(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	channel := uuid.NewString()
	seedChannel(t, pg, dg, channel, "maya", "outsider")

	key := helpers.CallSessionKeyFor(uuid.NewString())
	if err := liveKitBusiness.EnsureCallSessionNode(ctx, key, channel+"~b1"); err != nil {
		t.Fatalf("a channel call's breakout keeps no transcript: %v", err)
	}
	raw := dg.Query(t, `{
		session(func: eq(recording_egress_id, "`+key+`")) { recording_channel { ch_uuid } }
		stray(func: eq(ch_uuid, "`+channel+`~b1")) { uid }
	}`)
	var got struct {
		Session []struct {
			Channel json.RawMessage `json:"recording_channel"`
		} `json:"session"`
		Stray []json.RawMessage `json:"stray"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Session) != 1 || !strings.Contains(string(got.Session[0].Channel), `"`+channel+`"`) {
		t.Errorf("the breakout's transcript node: %s; want it filed under the channel %s", raw, channel)
	}
	if len(got.Stray) != 0 {
		t.Errorf("a breakout's room name made a channel of its own: %s", raw)
	}
}
