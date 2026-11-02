//go:build integration
// +build integration

package integration_test

// Settings > Archive brings a deleted recording back by the id its list gave
// (V-141), end to end over the real graph: a channel's recording and a call's
// transcript-only node, deleted from the channel (V-140's soft delete), are
// listed in the Archive by their keys (the egress id, and the call's session
// key), and a restore by those keys puts both back in the channel's list.
//
// The restore read every id as a uuid, so it refused a recording's key 400 and
// nothing in the Archive's recordings tab could be brought back. And the list
// listed any node holding an egress id and a deletion time, so nodes nobody
// can see (an old archive's leftover, a recording filed nowhere) were offered
// and could never come back. Now one filter decides both what the list shows
// and what a restore may name: a key naming nothing the Archive keeps is 404
// not_in_archive, and the batch it is in restores nothing.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTheArchiveBringsARecordingBackByItsListedKey -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	archiveController "github.com/akashc777/OneCamp/controllers/Archive"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheArchiveBringsARecordingBackByItsListedKey(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)

	// Keys as LiveKit writes them: EG_ and twelve letters and digits for an
	// egress, and "call-" and a room session id (RM_ and twelve) for a call's
	// transcript-only node.
	key := func(prefix string) string { return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
	recordingKey, sessionKey := key("EG_"), key("call-RM_")
	nowhereKey, nothingKey := key("EG_"), key("call-RM_")

	channel, samID, mayaID := uuid.NewString(), uuid.New(), uuid.New()
	started, ended, deleted := time.Now().Add(-3*time.Hour), time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": samID.String(), "user_name": "Sam Rivera"},
		{"uid": "_:maya", "dgraph.type": "User", "user_uuid": mayaID.String(), "user_name": "Maya Chen"},
		{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": channel, "ch_name": "launch",
			"ch_members":   []map[string]any{{"uid": "_:sam"}, {"uid": "_:maya"}},
			"ch_recording": []map[string]any{{"uid": "_:rec"}, {"uid": "_:session"}}},
		{"uid": "_:rec", "dgraph.type": "Recording", "recording_egress_id": recordingKey,
			"recording_stared_at": started, "recording_ended_at": ended,
			"recording_obj_key": "recordings/launch/" + recordingKey + ".mp4", "recording_size": 5000,
			"recording_channel": map[string]any{"uid": "_:ch"}},
		// A call nobody recorded keeps its words on a transcript-only node; one
		// the boot repair ended (MarkCallSessionNodeEnded) has no start time,
		// and the channel lists it all the same.
		{"uid": "_:session", "dgraph.type": "Recording", "recording_egress_id": sessionKey,
			"recording_transcript_only": true, "recording_ended_at": ended,
			"recording_channel": map[string]any{"uid": "_:ch"}},
		// What an old archive left: a node holding the recording's key and a time.
		{"uid": "_:leftover", "dgraph.type": "Recording", "recording_egress_id": recordingKey, "recording_deleted_at": deleted},
		// Recordings filed nowhere, under the recording's key and under a key of
		// their own: started, ended and deleted, but no channel, conversation or
		// event holds them, so no restore could bring one back where anyone sees it.
		{"uid": "_:nowhere", "dgraph.type": "Recording", "recording_egress_id": recordingKey,
			"recording_stared_at": started, "recording_ended_at": ended, "recording_deleted_at": deleted},
		{"uid": "_:nowhere_alone", "dgraph.type": "Recording", "recording_egress_id": nowhereKey,
			"recording_stared_at": started, "recording_ended_at": ended, "recording_deleted_at": deleted},
	})

	// Sam is a workspace admin and Maya a member; both are in #launch.
	person := func(id uuid.UUID, uid, name string, admin bool) userModels.UserInfo {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = id
		u.UserPostgresInfo.IsAdmin = admin
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uid, Uuid: id.String(), UserName: name}
		return u
	}
	sam, maya := person(samID, uids["sam"], "Sam Rivera", true), person(mayaID, uids["maya"], "Maya Chen", false)

	// call asks as who, through the routes this walk takes as the router
	// chains them: the sign-in chain puts who is asking in the request, then
	// the channel's routes, and the Archive's behind the admin gate.
	call := func(who userModels.UserInfo, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, who)))
			})
		})
		r.Get("/ch/getRecordingList/{channel_uuid}", channelController.GetAllChannelRecordingList)
		r.Post("/ch/deleteRecording/{egressId}", recordingController.HandleDeleteChannelRecording)
		r.Group(func(r chi.Router) {
			r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
			r.Get("/admin/archive/recent-items/{entityType}", archiveController.HandleGetRecentArchivedItems)
			r.Post("/admin/archive/restore", archiveController.HandleRestore)
		})
		var payload bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&payload).Encode(body); err != nil {
				t.Fatal(err)
			}
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, path, &payload))
		return rec
	}

	// inLaunch is the keys #launch lists, as Maya sees them.
	inLaunch := func() []string {
		t.Helper()
		rec := call(maya, http.MethodGet, "/ch/getRecordingList/"+channel, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("#launch's recordings answered %d: %s", rec.Code, rec.Body)
		}
		var body struct {
			ChannelInfo struct {
				Recordings []struct {
					Key string `json:"recording_egress_id"`
				} `json:"recordings"`
			} `json:"channel_info"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("#launch's recordings %q: %v", rec.Body, err)
		}
		var keys []string
		for _, r := range body.ChannelInfo.Recordings {
			keys = append(keys, r.Key)
		}
		return keys
	}

	// archive is the Archive's recordings tab as who opens it: its answer, and
	// the ids it lists, in its order.
	archive := func(who userModels.UserInfo) (int, []string) {
		t.Helper()
		rec := call(who, http.MethodGet, "/admin/archive/recent-items/recordings?limit=100", nil)
		if rec.Code != http.StatusOK {
			return rec.Code, nil
		}
		var body struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("the Archive's recordings %q: %v", rec.Body, err)
		}
		ids := make([]string, 0, len(body.Items))
		for _, it := range body.Items {
			ids = append(ids, it.ID)
		}
		return rec.Code, ids
	}
	times := func(ids []string, key string) int {
		n := 0
		for _, id := range ids {
			if id == key {
				n++
			}
		}
		return n
	}

	// restore asks the Archive to bring keys back, as who: its answer and body.
	restore := func(who userModels.UserInfo, keys ...string) (int, map[string]any) {
		t.Helper()
		rec := call(who, http.MethodPost, "/admin/archive/restore", map[string]any{"entity_type": "recordings", "entity_ids": keys})
		body := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	if got := inLaunch(); !slices.Contains(got, recordingKey) || !slices.Contains(got, sessionKey) {
		t.Fatalf("#launch lists %v, want %s and %s", got, recordingKey, sessionKey)
	}

	// Sam deletes both from #launch. The delete is soft (V-140): both leave the
	// channel and go to the Archive.
	for _, k := range []string{recordingKey, sessionKey} {
		if rec := call(sam, http.MethodPost, "/ch/deleteRecording/"+k, nil); rec.Code != http.StatusOK {
			t.Fatalf("deleting %s from #launch answered %d: %s", k, rec.Code, rec.Body)
		}
	}
	if got := inLaunch(); slices.Contains(got, recordingKey) || slices.Contains(got, sessionKey) {
		t.Fatalf("#launch still lists %v after both were deleted", got)
	}

	// The Archive is the workspace admins': Maya can't open it, and her
	// restore brings nothing back.
	if code, _ := archive(maya); code != http.StatusForbidden {
		t.Errorf("a member opened the Archive's recordings: %d, want 403", code)
	}
	if code, body := restore(maya, recordingKey); code != http.StatusForbidden || body["code"] != customMiddleware.AdminRequired {
		t.Errorf("a member's restore answered %d %v, want 403 %s", code, body, customMiddleware.AdminRequired)
	}
	if slices.Contains(inLaunch(), recordingKey) {
		t.Error("a member's restore brought the recording back to #launch")
	}

	// The Archive lists each deleted item once, by the key a restore takes, and
	// never what no restore can bring back: the old archive's leftover, or a
	// recording filed nowhere.
	code, listed := archive(sam)
	if code != http.StatusOK {
		t.Fatalf("the Archive's recordings answered %d", code)
	}
	for _, want := range []struct {
		key, what string
		n         int
	}{
		{recordingKey, "the recording (its leftover and its copy filed nowhere never)", 1},
		{sessionKey, "the call's transcript", 1},
		{nowhereKey, "a recording filed nowhere", 0},
	} {
		if n := times(listed, want.key); n != want.n {
			t.Errorf("the Archive lists %s (%s) %d times, want %d", want.what, want.key, n, want.n)
		}
	}

	// A key naming nothing the Archive keeps is refused, and so is the batch it
	// is in: nothing is restored. A key only a recording filed nowhere holds
	// names nothing the Archive keeps either.
	if code, body := restore(sam, recordingKey, nowhereKey); code != http.StatusNotFound || body["code"] != "not_in_archive" {
		t.Errorf("a batch with a key only a recording filed nowhere holds answered %d %v, want 404 not_in_archive", code, body)
	}
	if slices.Contains(inLaunch(), recordingKey) {
		t.Error("a refused batch brought the recording back to #launch")
	}
	if code, body := restore(sam, nothingKey); code != http.StatusNotFound || body["code"] != "not_in_archive" {
		t.Errorf("a key naming nothing answered %d %v, want 404 not_in_archive", code, body)
	}

	// Sam restores what the Archive listed, by the ids it gave: both are back
	// in #launch, and the Archive lists neither.
	var picked []string
	for _, id := range listed {
		if (id == recordingKey || id == sessionKey) && !slices.Contains(picked, id) {
			picked = append(picked, id)
		}
	}
	if code, body := restore(sam, picked...); code != http.StatusOK || body["count"] != float64(len(picked)) {
		t.Fatalf("restoring %v by the Archive's ids answered %d %v, want 200 and %d restored", picked, code, body, len(picked))
	}
	if got := inLaunch(); !slices.Contains(got, recordingKey) || !slices.Contains(got, sessionKey) {
		t.Errorf("#launch lists %v after the restore, want %s and %s back", got, recordingKey, sessionKey)
	}
	if _, after := archive(sam); times(after, recordingKey)+times(after, sessionKey)+times(after, nowhereKey) != 0 {
		t.Errorf("the Archive still lists %v after the restore", after)
	}
}
