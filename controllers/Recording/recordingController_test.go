package controllers

import (
	"context"
	"encoding/json"
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
	code, _ := askRecordingAnswer(handler, userUid, admin, egressId)
	return code
}

// askRecordingAnswer is askRecording with the answer's {"msg"}.
func askRecordingAnswer(handler http.HandlerFunc, userUid string, admin bool, egressId string) (int, string) {
	r := httptest.NewRequest(http.MethodPost, "/deleteRecording/"+egressId, nil)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("egressId", egressId)
	info := model.UserInfo{}
	info.UserDgraphInfo.Uid = userUid
	info.UserPostgresInfo.IsAdmin = admin
	ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
	w := httptest.NewRecorder()
	handler(w, r.WithContext(ctx))
	var answer struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &answer)
	return w.Code, answer.Msg
}

// outsiderAnsweredAsMissing is V-1126: someone who can't see a recording (not
// in its channel or conversation) is answered for it exactly as for a
// recording there isn't, same status and same words, and nothing changes.
func outsiderAnsweredAsMissing(t *testing.T, held recordingsHeld, handler http.HandlerFunc, verb, outsider string, admin bool) {
	t.Helper()
	before := held.deleted["EG_1"]
	code, msg := askRecordingAnswer(handler, outsider, admin, "EG_1")
	missingCode, missingMsg := askRecordingAnswer(handler, outsider, admin, "EG_missing")
	if code != http.StatusNotFound || code != missingCode || msg != missingMsg || held.deleted["EG_1"] != before {
		t.Errorf("%s by someone outside its place: %d %q (deleted: %v), want %d %q as for one there isn't, and nothing changed",
			verb, code, msg, held.deleted["EG_1"], missingCode, missingMsg)
	}
}

// keepRecordingSeams puts the handlers' reads and writes back after a test.
func keepRecordingSeams(t *testing.T) {
	a, b, c, d, e, f := liveChannelRecording, deletedChannelRecording, liveChatRecording, deletedChatRecording, deleteRecording, restoreRecording
	t.Cleanup(func() {
		liveChannelRecording, deletedChannelRecording, liveChatRecording, deletedChatRecording, deleteRecording, restoreRecording = a, b, c, d, e, f
	})
}

// A channel's recordings are deleted, and brought back, only by its moderators
// and the workspace's admins in it; its other members watch them and are told
// so (403), and someone outside it, a workspace admin too, is answered as if
// there were none (V-1126, V-1166). The delete is soft: a deleted recording is deleted again as
// not found, and its Undo brings it back once. A read or a write that failed
// is "try again", never "not found".
func TestOnlyAChannelsModeratorsDeleteItsRecordingsAndBringThemBack(t *testing.T) {
	keepRecordingSeams(t)
	const moderator, member, admin, outsider, adminOutside, wasModerator = "0x1", "0x2", "0x3", "0x4", "0x5", "0x6"
	held := recordingsHeld{deleted: map[string]bool{"EG_1": false}, standing: func(rec *dgraphStruct.DgraphRecording, userUid string) {
		rec.Channel = &dgraphStruct.DgraphChannel{IsMember: 1}
		switch userUid {
		case moderator:
			rec.Channel.IsAdmin = 1
		case outsider, adminOutside:
			rec.Channel.IsMember = 0
		case wasModerator: // taken out of the channel, still on its moderators
			rec.Channel.IsAdmin, rec.Channel.IsMember = 1, 0
		}
	}}
	liveChannelRecording, deletedChannelRecording = held.read(false), held.read(true)
	deleteRecording, restoreRecording = held.write(true), held.write(false)
	del, restore := HandleDeleteChannelRecording, HandleRestoreChannelRecording

	if code, msg := askRecordingAnswer(del, member, false, "EG_1"); code != http.StatusForbidden ||
		msg != "Only the channel's moderators can delete its recordings." || held.deleted["EG_1"] {
		t.Errorf("a member deleted: %d %q (deleted: %v), want 403 with the moderators' sentence and nothing deleted", code, msg, held.deleted["EG_1"])
	}
	outsiderAnsweredAsMissing(t, held, del, "a delete", outsider, false)
	if code := askRecording(del, moderator, false, "EG_2"); code != http.StatusNotFound {
		t.Errorf("a recording there isn't: %d, want 404", code)
	}
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusOK || !held.deleted["EG_1"] {
		t.Errorf("a moderator deleted: %d (deleted: %v), want 200 and it deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, moderator, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	if code, msg := askRecordingAnswer(restore, member, false, "EG_1"); code != http.StatusForbidden ||
		msg != "Only the channel's moderators can restore its recordings." || !held.deleted["EG_1"] {
		t.Errorf("a member brought it back: %d %q (deleted: %v), want 403 with the moderators' sentence and it still deleted", code, msg, held.deleted["EG_1"])
	}
	outsiderAnsweredAsMissing(t, held, restore, "a restore", outsider, false)
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
	// A workspace admin outside the channel can't see its recordings, so is
	// answered as if there were none, live or deleted, and nothing changes
	// (V-1166), as on their link and transcript.
	outsiderAnsweredAsMissing(t, held, del, "a delete, a workspace admin's,", adminOutside, true)
	held.deleted["EG_1"] = true
	outsiderAnsweredAsMissing(t, held, restore, "a restore, a workspace admin's,", adminOutside, true)
	held.deleted["EG_1"] = false
	// Nor does one taken out of the channel whom it still names a moderator.
	outsiderAnsweredAsMissing(t, held, del, "a delete, a former moderator's,", wasModerator, false)
	held.deleted["EG_1"] = true
	outsiderAnsweredAsMissing(t, held, restore, "a restore, a former moderator's,", wasModerator, false)
	held.deleted["EG_1"] = false

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
// people, and a group's by any of its members; nobody else, who is answered
// as if there were no recording (V-1126), a workspace admin too. The same
// routes' answers as a channel's: not found once deleted, and its Undo once.
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

	outsiderAnsweredAsMissing(t, held, del, "a delete, a workspace admin's,", outside, true)
	if code := askRecording(del, inside, false, "EG_1"); code != http.StatusOK || !held.deleted["EG_1"] {
		t.Errorf("someone in it deleted: %d (deleted: %v), want 200 and it deleted", code, held.deleted["EG_1"])
	}
	if code := askRecording(del, inside, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", code)
	}
	outsiderAnsweredAsMissing(t, held, restore, "a restore, a workspace admin's,", outside, true)
	if code := askRecording(restore, inside, false, "EG_1"); code != http.StatusOK || held.deleted["EG_1"] {
		t.Errorf("their Undo: %d (deleted: %v), want 200 and it back", code, held.deleted["EG_1"])
	}
	if code := askRecording(restore, inside, false, "EG_1"); code != http.StatusNotFound {
		t.Errorf("bringing back one that's there: %d, want 404", code)
	}
}
