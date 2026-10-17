//go:build integration
// +build integration

package integration_test

// An instant meeting's own page, /app/meet/m/<id>, said "This meeting has
// ended" for an id that no meeting ever had (QA F-54): the server answered a
// meeting that never was and one that is over with the same bare 404. Both
// stay 404s, which an older page still reads as "ended", and a code tells them
// apart: "no_meeting" and "ended". Someone else's meeting is still a 403.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAMeetingThatNeverWasIsNotOneThatEnded -v

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAMeetingThatNeverWasIsNotOneThatEnded(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	olu, ben := uuid.New(), uuid.New()
	for name, id := range map[string]uuid.UUID{"olu": olu, "ben": ben} {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	// A meeting olu started, as CreateInstantMeeting starts one: its first
	// link, without a call server to make the room on.
	meeting := func(t *testing.T) (string, uuid.UUID) {
		t.Helper()
		id := uuid.New()
		sum := sha256.Sum256([]byte("meeting-" + id.String()))
		expires := time.Now().Add(time.Hour)
		grant, err := guestModel.CreateGrant(ctx, sum[:], guestModel.ResourceMeeting, guestBusiness.MeetingRoomPrefix+id.String(), guestModel.CapabilityJoin, olu, &expires)
		if err != nil {
			t.Fatal(err)
		}
		return id.String(), grant
	}
	live, _ := meeting(t)
	over, overLink := meeting(t)
	if _, err := env.PG.Exec(`UPDATE guest_grants SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, overLink); err != nil {
		t.Fatal(err)
	}

	open := func(t *testing.T, as uuid.UUID, meetingID string) (int, string, string) {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("meeting_id", meetingID)
		who := userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: as.String()}}
		rctx := context.WithValue(context.WithValue(ctx, chi.RouteCtxKey, rc), helpers.UserInfoContextKey, who)
		rec := httptest.NewRecorder()
		guestController.GetInstantMeeting(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(rctx))
		var answer struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Code, answer.Msg
	}

	cases := []struct {
		what     string
		as       uuid.UUID
		id       string
		status   int
		code     string
		saysWhat string
	}{
		{"an id no meeting ever had", olu, uuid.NewString(), http.StatusNotFound, "no_meeting", "We couldn't find this meeting."},
		{"something that isn't a meeting id", olu, "not-a-meeting", http.StatusNotFound, "no_meeting", "We couldn't find this meeting."},
		{"a meeting whose link has run out", olu, over, http.StatusNotFound, "ended", "This meeting has ended."},
		{"someone else's meeting", ben, live, http.StatusForbidden, "not_host", "Only the person who started this meeting can open it here."},
	}
	for _, c := range cases {
		status, code, msg := open(t, c.as, c.id)
		if status != c.status || code != c.code || msg != c.saysWhat {
			t.Errorf("%s: %d %q %q, want %d %q %q", c.what, status, code, msg, c.status, c.code, c.saysWhat)
		}
	}
}
