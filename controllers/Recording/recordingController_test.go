package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

// readRecording is how the handlers read a recording: by its egress id, with
// the asker's standing in its home.
type readRecording = func(context.Context, string, string) (*dgraphStruct.DgraphRecording, error)

// recordingsHeld answers the handlers' reads and writes from a map of the
// recordings there are (true: deleted), with each asker's standing in their
// home as standing says.
type recordingsHeld struct {
	deleted  map[string]bool
	standing func(rec *dgraphStruct.DgraphRecording, userUid string)
}

// read finds a recording only in the state the read asks for (deleted or
// live), as the graph's reads do.
func (h recordingsHeld) read(deleted bool) readRecording {
	return func(_ context.Context, egressId, userUid string) (*dgraphStruct.DgraphRecording, error) {
		if gone, ok := h.deleted[egressId]; !ok || gone != deleted {
			return nil, recordingDomain.ErrNotFound
		}
		rec := &dgraphStruct.DgraphRecording{EgressId: egressId}
		h.standing(rec, userUid)
		return rec, nil
	}
}

func (h recordingsHeld) write(deleted bool) func(context.Context, string) error {
	return func(_ context.Context, egressId string) error {
		h.deleted[egressId] = deleted
		return nil
	}
}

// askRecording calls a handler as the member with this graph uid (a workspace
// admin when admin), for this egress id, and answers the status.
func askRecording(handler http.HandlerFunc, userUid string, admin bool, egressId string) int {
	r := httptest.NewRequest(http.MethodPost, "/deleteRecording/"+egressId, nil)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("egressId", egressId)
	info := model.UserInfo{}
	info.UserDgraphInfo.Uid = userUid
	info.UserPostgresInfo.IsAdmin = admin
	ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
	w := httptest.NewRecorder()
	handler(w, r.WithContext(ctx))
	return w.Code
}

// keepRecordingSeams puts the handlers' reads and writes back after a test.
func keepRecordingSeams(t *testing.T) {
	a, b, c, d, e, f := liveChannelRecording, deletedChannelRecording, liveChatRecording, deletedChatRecording, deleteRecording, restoreRecording
	t.Cleanup(func() {
		liveChannelRecording, deletedChannelRecording, liveChatRecording, deletedChatRecording, deleteRecording, restoreRecording = a, b, c, d, e, f
	})
}

// A channel's recordings are deleted, and brought back, only by its moderators
// and the workspace's admins; its members watch them. The delete is soft: a
// deleted recording is deleted again as not found, and its Undo brings it
// back once. A read or a write that failed is "try again", never "not found".
func TestOnlyAChannelsModeratorsDeleteItsRecordingsAndBringThemBack(t *testing.T) {
	keepRecordingSeams(t)
	const moderator, member, admin = "0x1", "0x2", "0x3"
	held := recordingsHeld{deleted: map[string]bool{"EG_1": false}, standing: func(rec *dgraphStruct.DgraphRecording, userUid string) {
		rec.Channel = &dgraphStruct.DgraphChannel{IsMember: 1}
		if userUid == moderator {
			rec.Channel.IsAdmin = 1
		}
	}}
	liveChannelRecording, deletedChannelRecording = held.read(false), held.read(true)
	deleteRecording, restoreRecording = held.write(true), held.write(false)
	del, restore := HandleDeleteChannelRecording, HandleRestoreChannelRecording

	if code := askRecording(del, member, false, "EG_1"); code != http.StatusForbidden || held.deleted["EG_1"] {
		t.Errorf("a member deleted: %d (deleted: %v), want 403 and nothing deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, moderator, false, "EG_2"); code != http.StatusNotFound {
		t.Errorf("a recording there isn't: %d, want 404", code)
	}
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusOK || !held.deleted["EG_1"] {
		t.Errorf("a moderator deleted: %d (deleted: %v), want 200 and it deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code := askRecording(restore, member, false, "EG_1"); code != http.StatusForbidden || !held.deleted["EG_1"] {
		t.Errorf("a member brought it back: %d (deleted: %v), want 403 and it still deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, moderator, false, "EG_1"); code != http.StatusOK || held.deleted["EG_1"] {
		t.Errorf("a moderator's Undo: %d (deleted: %v), want 200 and it back", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, moderator, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("bringing back one that's there: %d, want 404", code)
	}
	if code := askRecording(del, admin, true, "EG_1"); code != http.StatusOK || !held.deleted["EG_1"] {
		t.Errorf("a workspace admin deleted: %d (deleted: %v), want 200 and it deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, admin, true, "EG_1"); code != http.StatusOK || held.deleted["EG_1"] {
		t.Errorf("a workspace admin's Undo: %d (deleted: %v), want 200 and it back", code, held.deleted["EG_1"])
	}

	unreadable := func(context.Context, string, string) (*dgraphStruct.DgraphRecording, error) {
		return nil, errors.New("graph unreachable")
	}
	liveChannelRecording, deletedChannelRecording = unreadable, unreadable
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusServiceUnavailable {
		t.Errorf("a delete whose read failed: %d, want 503", code)
	}
	if code := askRecording(restore, moderator, false, "EG_1"); code != http.StatusServiceUnavailable {
		t.Errorf("a restore whose read failed: %d, want 503", code)
	}
	liveChannelRecording, deletedChannelRecording = held.read(false), held.read(true)
	unwritable := func(context.Context, string) error { return errors.New("graph unreachable") }
	deleteRecording, restoreRecording = unwritable, unwritable
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusServiceUnavailable || held.deleted["EG_1"] {
		t.Errorf("a delete whose write failed: %d, want 503", code)
	}
	held.deleted["EG_1"] = true
	if code := askRecording(restore, moderator, false, "EG_1"); code != http.StatusServiceUnavailable || !held.deleted["EG_1"] {
		t.Errorf("a restore whose write failed: %d, want 503", code)
	}
}

// A DM's recordings are deleted, and brought back, by either of its two
// people, and a group's by any of its members; nobody else. The same routes'
// answers as a channel's: not found once deleted, and its Undo once.
func TestOnlyPeopleInAConversationDeleteItsRecordingsAndBringThemBack(t *testing.T) {
	keepRecordingSeams(t)
	const inside, outside = "0x1", "0x2"
	held := recordingsHeld{deleted: map[string]bool{"EG_1": false}, standing: func(rec *dgraphStruct.DgraphRecording, userUid string) {
		rec.Dm = &dgraphStruct.DgraphDm{}
		if userUid == inside {
			rec.Dm.ParticipantIsMember = 1
		}
	}}
	liveChatRecording, deletedChatRecording = held.read(false), held.read(true)
	deleteRecording, restoreRecording = held.write(true), held.write(false)
	del, restore := HandleDeleteChatRecording, HandleRestoreChatRecording

	if code := askRecording(del, outside, true, "EG_1"); code != http.StatusForbidden || held.deleted["EG_1"] {
		t.Errorf("someone outside the conversation, a workspace admin, deleted: %d (deleted: %v), want 403", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, inside, false, "EG_1"); code != http.StatusOK || !held.deleted["EG_1"] {
		t.Errorf("someone in it deleted: %d (deleted: %v), want 200 and it deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, inside, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code := askRecording(restore, outside, true, "EG_1"); code != http.StatusForbidden || !held.deleted["EG_1"] {
		t.Errorf("someone outside it brought it back: %d (deleted: %v), want 403 and it still deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, inside, false, "EG_1"); code != http.StatusOK || held.deleted["EG_1"] {
		t.Errorf("their Undo: %d (deleted: %v), want 200 and it back", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, inside, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("bringing back one that's there: %d, want 404", code)
	}
}
