//go:build integration

package integration_test

// zoom-09: a person's own OK or Leave on a recorded call's notice is a row in
// the admin audit log, over the real routes: who (from their session or their
// own call token, never the body), what was on (from the room), and the
// choice. Only someone in the call writes one, never through an API token, a
// stale notice agrees to nothing, the same answer is one row, and admins alone
// read them.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	callsController "github.com/akashc777/OneCamp/controllers/Calls"
	settingsController "github.com/akashc777/OneCamp/controllers/Settings"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/livekitInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallConsentRecordsWhoAgreed(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	integration.StubMqttClient(t)
	lk := integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}

	// Maya hosts a meeting and Gia joins it as a guest; Ben is never in it.
	people := seedChannel(t, pg, dg, uuid.NewString(), "maya", "ben")
	meeting, err := guestBusiness.CreateInstantMeeting(ctx, people["maya"], "")
	if err != nil {
		t.Fatal(err)
	}
	host := join(t, lk, meeting.HostToken)
	joinSettled(t, meeting.Room, people["maya"].Uid, host)
	connected(t, ctx, meeting.Room, people["maya"].Uid)
	door, err := guestBusiness.OpenDoor(ctx, meeting.GuestToken)
	if err != nil {
		t.Fatal(err)
	}
	guestToken, _, err := guestBusiness.IssueGuestMeetingToken(ctx, door, "Gia", "")
	if err != nil {
		t.Fatal(err)
	}
	gia := join(t, lk, guestToken)
	giaID, _, err := callControlBusiness.GuestOf(guestToken)
	if err != nil {
		t.Fatal(err)
	}
	joinSettled(t, meeting.Room, giaID, gia)
	connected(t, ctx, meeting.Room, giaID)
	s, err := callStateBusiness.Current(ctx, callStateBusiness.Ref{Kind: callStateBusiness.KindMeeting, ID: meeting.MeetingID})
	if err != nil || s == nil {
		t.Fatalf("no meeting recorded: %v", err)
	}
	// The call is recorded, as the server writes it once the egress runs.
	if _, err := livekitInit.LiveKitService.UpdateRoomMetadata(ctx, meeting.Room, map[string]any{"isRecording": true, "recordingStartedBy": "Maya", "egressID": "EG_c1"}); err != nil {
		t.Fatal(err)
	}

	signedIn := func(u *dgraphStruct.DgraphUser, admin bool) context.Context {
		t.Helper()
		row, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, uuid.MustParse(u.Uuid))
		if err != nil || row == nil {
			t.Fatalf("load %s: %v", u.UserFullName, err)
		}
		row.IsAdmin = admin
		return context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *row, UserDgraphInfo: *u})
	}
	routes := chi.NewRouter()
	routes.Post("/calls/{session_id}/consent", callsController.RecordCallConsent)
	routes.Post("/guest/calls/consent", callsController.GuestRecordCallConsent)
	routes.With(customMiddleware.VerifyAdminAuthOnlyPostgres).Get("/admin/audit-log", settingsController.GetAuditLog)
	// One answer through the routes: its status, its refusal's code, and
	// whether a row was written.
	answer := func(as context.Context, path, auth, body string) (int, string, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body))).WithContext(as)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var got struct {
			Code string `json:"code"`
			Data struct {
				Recorded bool `json:"recorded"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got.Code, got.Data.Recorded
	}
	member := "/calls/" + s.ID + "/consent"
	guest := "/guest/calls/consent"
	ok := `{"choice":"accepted","recording":"EG_c1","transcript":0}`
	maya := signedIn(people["maya"], true)

	t.Run("a member accepts, once", func(t *testing.T) {
		if code, why, rec := answer(maya, member, "", ok); code != http.StatusOK || !rec {
			t.Errorf("Maya's OK: %d %s recorded %v", code, why, rec)
		}
		if code, _, rec := answer(maya, member, "", ok); code != http.StatusOK || rec {
			t.Errorf("Maya's OK again: %d recorded %v, want no second row", code, rec)
		}
	})
	t.Run("a guest accepts, then leaves", func(t *testing.T) {
		if code, why, rec := answer(ctx, guest, "Bearer "+guestToken, ok); code != http.StatusOK || !rec {
			t.Errorf("Gia's OK: %d %s recorded %v", code, why, rec)
		}
		if code, why, rec := answer(ctx, guest, "Bearer "+guestToken, `{"choice":"left","recording":"EG_c1","transcript":0}`); code != http.StatusOK || !rec {
			t.Errorf("Gia leaving: %d %s recorded %v", code, why, rec)
		}
	})
	t.Run("nobody else writes one", func(t *testing.T) {
		if code, why, _ := answer(signedIn(people["ben"], false), member, "", ok); code != http.StatusForbidden || why != "not_in_call" {
			t.Errorf("Ben, not in the call: %d %s", code, why)
		}
		if code, why, _ := answer(helpers.WithActsThroughToken(maya), member, "", ok); code != http.StatusForbidden || why != "not_through_token" {
			t.Errorf("Maya's API token: %d %s", code, why)
		}
		if code, why, _ := answer(ctx, guest, "Bearer "+meeting.HostToken, ok); code != http.StatusForbidden || why != "not_in_call" {
			t.Errorf("a member's call token on the guest route: %d %s", code, why)
		}
		if code, why, _ := answer(maya, member, "", `{"choice":"accepted","recording":"EG_old","transcript":0}`); code != http.StatusConflict || why != "notice_stale" {
			t.Errorf("a notice of another recording: %d %s", code, why)
		}
	})

	t.Run("admins read the rows; nobody else does", func(t *testing.T) {
		read := func(as context.Context) (int, []byte) {
			rec := httptest.NewRecorder()
			routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/audit-log?category=calls&limit=200", nil).WithContext(as))
			return rec.Code, rec.Body.Bytes()
		}
		if code, _ := read(signedIn(people["ben"], false)); code != http.StatusForbidden {
			t.Errorf("a member reading the log: %d", code)
		}
		code, raw := read(maya)
		var page struct {
			Data struct {
				Entries []struct {
					Action   string     `json:"action"`
					ActorID  *uuid.UUID `json:"actor_id"`
					Summary  string     `json:"summary"`
					Metadata *string    `json:"metadata"`
				} `json:"entries"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &page); code != http.StatusOK || err != nil {
			t.Fatalf("an admin reading the log: %d %v", code, err)
		}
		var rows []string
		for _, e := range page.Data.Entries {
			if !strings.HasPrefix(e.Action, "call.privacy.consent_") || e.Metadata == nil || !strings.Contains(*e.Metadata, s.ID) {
				continue
			}
			var m struct {
				Identity  string `json:"identity"`
				Guest     bool   `json:"guest"`
				Recording struct {
					Egress string `json:"egress"`
				} `json:"recording"`
			}
			_ = json.Unmarshal([]byte(*e.Metadata), &m)
			if m.Recording.Egress != "EG_c1" {
				t.Errorf("%s: the recording from the room %q", e.Action, m.Recording.Egress)
			}
			if m.Guest != (e.ActorID == nil) {
				t.Errorf("%s: a guest's row has no member (guest %v, actor %v)", e.Action, m.Guest, e.ActorID)
			}
			rows = append(rows, strings.TrimPrefix(e.Action, "call.privacy.")+" "+m.Identity)
		}
		want := map[string]bool{"consent_accepted " + people["maya"].Uid: true, "consent_accepted " + giaID: true, "consent_left " + giaID: true}
		if len(rows) != len(want) {
			t.Errorf("the consent rows: %v, want %d", rows, len(want))
		}
		for _, r := range rows {
			if !want[r] {
				t.Errorf("an unexpected row %q", r)
			}
		}
		if res, err := auditBusiness.VerifyRecent(ctx, 50); err != nil || res == nil || !res.OK {
			t.Errorf("the chain after the rows: %+v %v", res, err)
		}
	})
}
