//go:build integration

package integration_test

// The recording stage admits only its recorder (H3, meet-17, main's security
// condition of 6 Nov). The web's layout page draws a live call, so the
// recorder reaches it only through the API's recording stage, with a ticket
// minted for that one recording: signed, bound to its room, good for five
// minutes and once, and dead as soon as its recording ends or the room
// records again. The stage checks it before anything of the call is handed
// out, then sends the recorder on to the page with a two-minute recorder's
// token in the address's fragment. Anything else (no ticket, a forged one,
// another room's, an expired, used or revoked one) gets a bare 404.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	lka "github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	livekitController "github.com/akashc777/OneCamp/controllers/LiveKit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// stageEntrance is where the recorder is sent: the API's recording stage.
const stageEntrance = "https://api.example.test/recording-stage/"

// ticketPurpose is the key the stage's tickets are sealed under, for the
// forgeries below: a test holds the server's secret, as nobody else does.
const ticketPurpose = "onecamp.recording-stage.v1"

// openStage asks the recording stage with ticket as LiveKit's recorder does:
// egress adds its own layout, server and 24-hour token to the address's query.
func openStage(ticket string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/recording-stage/x?layout=&url=ws%3A%2F%2Flivekit%3A7880&token=egress-own-token", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("ticket", ticket)
	rec := httptest.NewRecorder()
	livekitController.OpenRecordingStage(rec, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	return rec
}

// stageRefused says why a stage's answer isn't a bare 404 that tells nothing.
func stageRefused(rec *httptest.ResponseRecorder) string {
	switch {
	case rec.Code != http.StatusNotFound:
		return "status " + http.StatusText(rec.Code)
	case rec.Body.Len() != 0:
		return "a body"
	case rec.Header().Get("Location") != "":
		return "a Location"
	case rec.Header().Get("Cache-Control") != "no-store":
		return "a cacheable answer"
	}
	return ""
}

// ticketOf is the ticket an egress request's address carries: never printed.
func ticketOf(t *testing.T, info *livekit.EgressInfo) string {
	t.Helper()
	req := info.GetRoomComposite()
	ticket, ok := strings.CutPrefix(req.GetCustomBaseUrl(), stageEntrance)
	if !ok || ticket == "" || strings.ContainsAny(ticket, "/?#&") || req.GetLayout() != "" {
		t.Fatalf("the recorder opens an address of %d characters on layout %q, want the API's recording stage with a ticket alone",
			len(req.GetCustomBaseUrl()), req.GetLayout())
	}
	return ticket
}

// resealed is ticket sealed again with some of its fields changed, as only a
// holder of the server's secret could.
func resealed(t *testing.T, ticket string, change map[string]any) string {
	t.Helper()
	var body map[string]any
	if err := helpers.Unseal(ticketPurpose, ticket, &body); err != nil {
		t.Fatalf("the ticket doesn't open under %s: %v", ticketPurpose, err)
	}
	for k, v := range change {
		body[k] = v
	}
	forged, err := helpers.Seal(ticketPurpose, body)
	if err != nil {
		t.Fatal(err)
	}
	return forged
}

// admitted checks a stage's answer to a good ticket for room: a 302 to the
// web's layout page, uncached and unreferred, with this LiveKit server and a
// recorder's token for room alone in its fragment, and nothing of egress's own.
func admitted(t *testing.T, rec *httptest.ResponseRecorder, room, lkURL, secret string) {
	t.Helper()
	if rec.Code != http.StatusFound || rec.Body.Len() != 0 {
		t.Fatalf("a good ticket: %d with %d bytes, want a bare 302", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("the 302 says Cache-Control %q, Referrer-Policy %q: want no-store, no-referrer",
			rec.Header().Get("Cache-Control"), rec.Header().Get("Referrer-Policy"))
	}
	page, fragment, ok := strings.Cut(rec.Header().Get("Location"), "#")
	if !ok || page != "https://team.example.test/call-recording-layout" {
		t.Fatalf("the 302 goes to %q, want the web's layout page with a fragment", page)
	}
	q, err := url.ParseQuery(fragment)
	if err != nil || len(q) != 2 || strings.Contains(fragment, "egress-own-token") {
		t.Fatalf("the fragment has %d keys (%v), want lk and t alone", len(q), err)
	}
	if want := strings.Replace(lkURL, "http", "ws", 1); q.Get("lk") != want {
		t.Errorf("the fragment's LiveKit is %q, want %q", q.Get("lk"), want)
	}
	v, err := lka.ParseAPIToken(q.Get("t"))
	if err != nil {
		t.Fatalf("the fragment's token: %v", err)
	}
	claims, err := v.Verify(secret)
	if err != nil {
		t.Fatalf("the fragment's token isn't this LiveKit's: %v", err)
	}
	g := claims.Video
	if g == nil || g.Room != room || !g.RoomJoin || !g.Hidden || !g.Recorder || !g.GetCanSubscribe() || g.GetCanPublish() || g.GetCanPublishData() || g.RoomAdmin {
		t.Errorf("the recorder's grant: %+v, want joining its room alone, hidden, recording, subscribing alone", g)
	}
	if claims.GetParticipantKind() != livekit.ParticipantInfo_EGRESS {
		t.Errorf("the recorder joins as %v, want an egress", claims.GetParticipantKind())
	}
	part := strings.Split(q.Get("t"), ".")
	body, _ := base64.RawURLEncoding.DecodeString(part[1])
	var times struct{ Exp int64 }
	_ = json.Unmarshal(body, &times)
	if life := time.Until(time.Unix(times.Exp, 0)); life <= 0 || life > 2*time.Minute {
		t.Errorf("the recorder's token lasts %v, want at most two minutes", life)
	}
}

func TestTheRecordingStageAdmitsOnlyItsRecorder(t *testing.T) {
	ctx, pg, dg, lk := breakoutEnv(t)
	egress := lk.UseFakeEgress(t)
	t.Setenv("FE_HOST_DOMAIN", "")
	t.Setenv("FRONTEND_DOMAIN", "team.example.test")
	t.Setenv("BACKEND_DOMAIN", "api.example.test")
	t.Setenv("JWT_SECRET", "the integration server's secret")

	// recording starts a stage recording of a call of its own, and answers
	// its room, its egress and its people.
	recording := func(t *testing.T) (string, *livekit.EgressInfo, map[string]*dgraphStruct.DgraphUser) {
		t.Helper()
		room := uuid.NewString()
		people := stageChannel(t, pg, dg, room)
		enterCall(t, ctx, lk, room, people, "Asha", "Ben")
		return room, startRecordingOf(t, egress, room, people["Asha"]), people
	}

	t.Run("a good ticket, once", func(t *testing.T) {
		room, info, _ := recording(t)
		ticket := ticketOf(t, info)
		r, err := livekitInit.LiveKitService.GetRoom(ctx, room)
		if err != nil || strings.Contains(r.Metadata, ticket) {
			t.Errorf("the room's metadata, which everyone in the call reads, carries the ticket (%v)", err)
		}
		admitted(t, openStage(ticket), room, lk.URL, lk.Secret)
		if why := stageRefused(openStage(ticket)); why != "" {
			t.Errorf("a used ticket is answered with %s, want a bare 404", why)
		}
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})

	t.Run("no ticket, or a forged one", func(t *testing.T) {
		_, info, _ := recording(t)
		ticket := ticketOf(t, info)
		body, sig, _ := strings.Cut(ticket, ".")
		for name, bad := range map[string]string{
			"none":          "",
			"no seal":       body,
			"a stolen seal": base64.RawURLEncoding.EncodeToString([]byte(`{"r":"another","n":"x","e":4102444800}`)) + "." + sig,
			"garbage":       "not-a-ticket",
		} {
			if why := stageRefused(openStage(bad)); why != "" {
				t.Errorf("%s: answered with %s, want a bare 404", name, why)
			}
		}
		// None of them used the good one up.
		admitted(t, openStage(ticket), info.RoomName, lk.URL, lk.Secret)
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})

	t.Run("a wrong room", func(t *testing.T) {
		_, first, _ := recording(t)
		room2, second, _ := recording(t)
		// The first recording's ticket, sealed for the second's room: the
		// second room's ticket is another, so it isn't this one.
		if why := stageRefused(openStage(resealed(t, ticketOf(t, first), map[string]any{"r": room2}))); why != "" {
			t.Errorf("another room's ticket: answered with %s, want a bare 404", why)
		}
		admitted(t, openStage(ticketOf(t, second)), room2, lk.URL, lk.Secret)
		endRecording(t, lk, egress, first.EgressId)
		endRecording(t, lk, egress, second.EgressId)
		settle(t)
	})

	t.Run("an expired ticket", func(t *testing.T) {
		room, info, _ := recording(t)
		ticket := ticketOf(t, info)
		if why := stageRefused(openStage(resealed(t, ticket, map[string]any{"e": time.Now().Add(-time.Second).Unix()}))); why != "" {
			t.Errorf("an expired ticket: answered with %s, want a bare 404", why)
		}
		var body struct{ E int64 }
		_ = helpers.Unseal(ticketPurpose, ticket, &body)
		if life := time.Until(time.Unix(body.E, 0)); life <= 0 || life > 5*time.Minute {
			t.Errorf("a ticket is good for %v, want at most five minutes", life)
		}
		admitted(t, openStage(ticket), room, lk.URL, lk.Secret)
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})

	t.Run("a stopped recording", func(t *testing.T) {
		room, info, people := recording(t)
		if code := recordingRoute(t, false, room, people["Asha"]); code != http.StatusOK {
			t.Fatalf("stopping the recording: %d", code)
		}
		if why := stageRefused(openStage(ticketOf(t, info))); why != "" {
			t.Errorf("a stopped recording's ticket: answered with %s, want a bare 404", why)
		}
		settle(t)
	})

	t.Run("an ended recording", func(t *testing.T) {
		// LiveKit says it ended (its room closed, its file is up): the
		// room's metadata may still say it records, the ticket is dead.
		_, info, _ := recording(t)
		ticket := ticketOf(t, info)
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
		if why := stageRefused(openStage(ticket)); why != "" {
			t.Errorf("an ended recording's ticket: answered with %s, want a bare 404", why)
		}
	})

	t.Run("a room that records again", func(t *testing.T) {
		room, first, _ := recording(t)
		ticket := ticketOf(t, first)
		// The first egress is gone without a word to this server, and the
		// room records again: only the newer recording's ticket is good.
		egress.End(t, first.EgressId)
		again, _, err := livekitInit.LiveKitService.StartRecording(ctx, room, "Asha")
		if err != nil {
			t.Fatal(err)
		}
		if why := stageRefused(openStage(ticket)); why != "" {
			t.Errorf("the earlier recording's ticket: answered with %s, want a bare 404", why)
		}
		admitted(t, openStage(ticketOf(t, again)), room, lk.URL, lk.Secret)
		endRecording(t, lk, egress, again.EgressId)
		settle(t)
	})

	// Redis alone says whether a recording's ticket is live (B29's B1): its
	// recorder is sent at once, and the room's metadata, which LiveKit shows
	// a moment after it is written, is no gate of the stage's.
	t.Run("a recorder that opens its ticket before the start is done", func(t *testing.T) {
		room := uuid.NewString()
		people := stageChannel(t, pg, dg, room)
		enterCall(t, ctx, lk, room, people, "Asha", "Ben")
		var early *httptest.ResponseRecorder
		egress.Starting = func(req *livekit.RoomCompositeEgressRequest) error {
			early = openStage(ticketOf(t, &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: req}}))
			return nil
		}
		t.Cleanup(func() { egress.Starting = nil })
		info := startRecordingOf(t, egress, room, people["Asha"])
		if early == nil {
			t.Fatal("the recorder never opened its ticket")
		}
		admitted(t, early, room, lk.URL, lk.Secret)
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})

	t.Run("a recorder the room's metadata hasn't caught up with", func(t *testing.T) {
		room, info, _ := recording(t)
		if _, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, room, map[string]any{"isRecording": false}); err != nil {
			t.Fatal(err)
		}
		admitted(t, openStage(ticketOf(t, info)), room, lk.URL, lk.Secret)
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})

	t.Run("a start the egress refused", func(t *testing.T) {
		room := uuid.NewString()
		people := stageChannel(t, pg, dg, room)
		enterCall(t, ctx, lk, room, people, "Asha", "Ben")
		ticket := ""
		egress.Starting = func(req *livekit.RoomCompositeEgressRequest) error {
			ticket = ticketOf(t, &livekit.EgressInfo{Request: &livekit.EgressInfo_RoomComposite{RoomComposite: req}})
			return errors.New("no egress worker can take it")
		}
		t.Cleanup(func() { egress.Starting = nil })
		if code := recordingRoute(t, true, room, people["Asha"]); code == http.StatusOK {
			t.Fatal("a start the egress refused answered 200")
		}
		if why := stageRefused(openStage(ticket)); why != "" {
			t.Errorf("a refused start's ticket: answered with %s, want a bare 404", why)
		}
	})

	t.Run("a stop after its egress ended unheard", func(t *testing.T) {
		// The egress is gone without a word to this server (its webhook is
		// late or lost): a stop still kills the ticket, whatever it finds.
		room, info, people := recording(t)
		egress.End(t, info.EgressId)
		recordingRoute(t, false, room, people["Asha"])
		if why := stageRefused(openStage(ticketOf(t, info))); why != "" {
			t.Errorf("the ticket after a stop: answered with %s, want a bare 404", why)
		}
		settle(t)
	})

	t.Run("an API on this machine records the grid", func(t *testing.T) {
		t.Setenv("BACKEND_DOMAIN", "localhost:3000")
		_, info, _ := recording(t)
		if req := info.GetRoomComposite(); req.GetCustomBaseUrl() != "" || req.GetLayout() != "grid-light" {
			t.Errorf("with the API on localhost the recorder opens %d characters on %q, want LiveKit's grid-light", len(req.GetCustomBaseUrl()), req.GetLayout())
		}
		endRecording(t, lk, egress, info.EgressId)
		settle(t)
	})
}
