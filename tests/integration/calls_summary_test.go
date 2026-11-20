//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	callInviteBusiness "github.com/akashc777/OneCamp/business/CallInvite"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	aiController "github.com/akashc777/OneCamp/controllers/AI"
	livekitController "github.com/akashc777/OneCamp/controllers/LiveKit"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// V-951. Everyone in a call, a guest as much as a member, reads from the
// call's room whether its summary (the AI's meeting recap) will be made: yes
// only while AI and the recap are both on and the call keeps its transcript.
// An admin's change reaches a call already going on without anyone flipping
// the switch again, and AI off says nothing. Through the routes: the member's
// flip and the admin's two settings.
func TestCallEveryoneReadsWhetherItsSummaryIsMade(t *testing.T) {
	ctx := context.Background()
	t.Setenv("JWT_SECRET", "the call summary test's secret")
	t.Setenv("TRANSCRIPTION_MODE", "frontend")
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	lk := integration.SetupLiveKit(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsBusiness.SetGuestAccessEnabled(false) })

	// OneCamp AI on, against a stand-in local model; the recap off, as a new
	// workspace has it.
	ollama := httptest.NewServer(fakeOllama{})
	defer ollama.Close()
	t.Setenv("OLLAMA_HOST", ollama.URL)
	if _, err := pg.PG.Exec(`UPDATE ai_settings SET embedding_dimension = $1, meeting_recap_enabled = false`, fakeEmbeddingDim); err != nil {
		t.Fatal(err)
	}
	if err := ai.InitAIService(); err != nil {
		t.Fatalf("init AI: %v", err)
	}
	if !ai.GetService().IsEnabled() {
		t.Fatal("AI did not come up enabled; nothing below would run")
	}
	t.Cleanup(func() {
		_, _ = pg.PG.Exec(`UPDATE ai_settings SET enabled = false, meeting_recap_enabled = false`)
		_ = ai.ReloadAIService(context.Background())
	})

	// Maya calls in her channel, and Gia joins as her guest.
	room := uuid.NewString()
	maya := seedChannel(t, pg, dg, room, "maya")["maya"]
	mayaToken, _, err := liveKitBusiness.CreateRoomAndGetToken(ctx, room, maya, false, "")
	if err != nil {
		t.Fatal(err)
	}
	mayaIn := join(t, lk, mayaToken)
	joinSettled(t, room, maya.Uid, mayaIn)
	connected(t, ctx, room, maya.Uid)
	eventually(t, "the call to begin", func() bool {
		s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: "channel", ID: room})
		return err == nil && s != nil
	})
	link, err := callInviteBusiness.Invite(ctx, room, maya)
	if err != nil {
		t.Fatal(err)
	}
	door, err := guestBusiness.OpenDoor(ctx, link.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	giaIn := join(t, lk, guestToken)

	mayaPG, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uuid.MustParse(maya.Uuid))
	if err != nil || mayaPG == nil {
		t.Fatalf("load Maya: %v", err)
	}
	as := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *mayaPG, UserDgraphInfo: *maya})
	post := func(route http.HandlerFunc, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		route(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(as))
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	// Both read the switch, on and kept, saying want of the summary: what
	// each one's client has of the room, as LiveKit sent it.
	both := func(want bool) {
		t.Helper()
		for who, j := range map[string]*joined{"Maya, a member": mayaIn, "Gia, a guest": giaIn} {
			eventually(t, fmt.Sprintf("%s to read summary %v", who, want), func() bool {
				var meta struct {
					Transcription *struct{ On, Kept, Summary bool } `json:"transcription"`
				}
				if json.Unmarshal([]byte(j.room.Metadata()), &meta) != nil || meta.Transcription == nil {
					return false
				}
				sw := meta.Transcription
				return sw.On && sw.Kept && sw.Summary == want
			})
		}
	}

	post(livekitController.SetCallTranscription, map[string]any{"room_name": room, "on": true})
	both(false) // the recap is off
	post(aiController.SetMeetingRecap, map[string]any{"enabled": true})
	both(true) // nobody flipped again
	post(aiController.SetEnabled, map[string]any{"enabled": false})
	both(false) // AI off says nothing
	post(aiController.SetEnabled, map[string]any{"enabled": true})
	both(true)
	post(aiController.SetMeetingRecap, map[string]any{"enabled": false})
	both(false)
}
