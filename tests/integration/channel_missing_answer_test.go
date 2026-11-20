//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Every channel route that reads the channel and then refuses someone outside
// it answers a channel there isn't exactly as it answers a private channel the
// asker isn't in: the same status, headers and body, nothing of the read's
// error in it, so nobody learns from it whether a channel exists (V-1163). A
// workspace admin outside the channel is answered the same way.
func TestNobodyOutsideAChannelLearnsWhetherItIsThere(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "channel-missing-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	lee := seedCalendarPerson(t, env.PG, dg, "Lee")
	leeAdmin := *lee
	leeAdmin.UserPostgresInfo.IsAdmin = true

	vault := uuid.NewString()
	dg.Mutate(t, map[string]any{"uid": "_:vault", "dgraph.type": "Channel", "ch_uuid": vault, "ch_name": "vault",
		"ch_private": true, "ch_created_at": time.Now().UTC().Format(time.RFC3339), "ch_deleted_at": "0001-01-01T00:00:00Z",
		"ch_created_by": map[string]any{"uid": sam.UserDgraphInfo.Uid},
		"ch_members":    []map[string]any{{"uid": sam.UserDgraphInfo.Uid}}, "ch_moderators": []map[string]any{{"uid": sam.UserDgraphInfo.Uid}}})

	type route struct {
		name, method, path string
		h                  http.HandlerFunc
		inPath             bool   // the channel id is the path's channel_uuid
		body               string // the channel id is the body's ID
	}
	routes := []route{
		{"channel info with flags", http.MethodGet, "/ch/channelInfoWithMemberAdminFlag/ID", channelController.GetChannelInfoByUUIDWithMemberAdminFlag, true, ""},
		{"mark seen", http.MethodPost, "/ch/markSeen/ID", channelController.MarkChannelSeen, true, ""},
		{"update info", http.MethodPost, "/ch/updateInfo", channelController.UpdateChannelInfo, false, `{"channel_uuid":"ID","channel_name":"renamed"}`},
		{"add moderator", http.MethodPost, "/ch/addModerator", channelController.AddChannelModerators, false,
			`{"channel_id":"ID","user_id":"` + lee.UserDgraphInfo.Uuid + `"}`},
		{"huddle token", http.MethodPost, "/ch/getCallToken", channelController.MakeVideoChannelCall, false, `{"channel_uuid":"ID"}`},
		{"start recording", http.MethodPost, "/ch/startCallRecording", channelController.StartVideoChannelCallRecording, false, `{"channel_uuid":"ID"}`},
		{"stop recording", http.MethodPost, "/ch/stopCallRecording", channelController.StopVideoChannelCallRecording, false, `{"channel_uuid":"ID"}`},
	}
	ask := func(r route, id string, who *userModels.UserInfo) *httptest.ResponseRecorder {
		var params map[string]string
		if r.inPath {
			params = map[string]string{"channel_uuid": id}
		}
		return call(t, r.h, r.method, strings.ReplaceAll(r.path, "ID", id), who, strings.ReplaceAll(r.body, "ID", id), params)
	}
	for _, who := range []struct {
		name string
		u    *userModels.UserInfo
	}{{"Lee, outside vault", lee}, {"Lee, a workspace admin outside vault", &leeAdmin}} {
		for _, r := range routes {
			there, missing := ask(r, vault, who.u), ask(r, uuid.NewString(), who.u)
			if there.Code < http.StatusBadRequest {
				t.Errorf("%s, %s: %d, want vault closed to them", r.name, who.name, there.Code)
			}
			if there.Code != missing.Code || !reflect.DeepEqual(there.Header(), missing.Header()) || !bytes.Equal(there.Body.Bytes(), missing.Body.Bytes()) {
				t.Errorf("%s, %s, vault against a channel there isn't: %d %v %s, want %d %v %s", r.name, who.name,
					there.Code, there.Header(), strings.TrimSpace(there.Body.String()), missing.Code, missing.Header(), strings.TrimSpace(missing.Body.String()))
			}
		}
	}
}
