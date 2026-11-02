package domain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A delete and a restore reach every node a list shows under these egress
// ids: a recording, which has a start from the moment it starts, and a call's
// transcript-only node, which the boot repair may have ended with no start
// (MarkCallSessionNodeEnded) and which the lists show all the same. A stray
// node an old archive or delete made (an id and a time, nothing else) is
// never touched. The ids are query variables, never query text.
func TestDeletesAndRestoresReachEveryNodeAListShows(t *testing.T) {
	query, vars := recordingsDeletedAtQuery([]string{"EG_1", "call-RM_2"})
	if !strings.Contains(query, "@filter(has(recording_stared_at) OR eq(recording_transcript_only, true))") {
		t.Errorf("a transcript-only node with no start isn't reached:\n%s", query)
	}
	if strings.Contains(query, "EG_1") || strings.Contains(query, "call-RM_2") || vars["$e0"] != "EG_1" || vars["$e1"] != "call-RM_2" {
		t.Errorf("the ids aren't query variables: %v\n%s", vars, query)
	}
}

// A recording is read for its delete while it is live and for its restore
// once it is deleted: ended, filed under its home (a stray node with the same
// egress id has none, so it is never the one read), with the asker's standing
// there. The ids are query variables, never query text.
func TestARecordingIsReadForItsRestoreOnlyOnceDeleted(t *testing.T) {
	was := recordingInfoRead
	t.Cleanup(func() { recordingInfoRead = was })
	var asked string
	var vars map[string]string
	recordingInfoRead = func(_ context.Context, query string, variables map[string]string) (*dgraphStruct.DgraphRecording, error) {
		asked, vars = query, variables
		return nil, ErrNotFound
	}
	live := dgraphquery.Live("recording_deleted_at")
	for _, c := range []struct {
		name     string
		read     func(context.Context, string, string) (*dgraphStruct.DgraphRecording, error)
		home     string
		standing string
		deleted  bool
	}{
		{"a channel's delete", GetDgraphChannelRecordingInfoByEgressId, "recording_channel", "ch_is_admin: count(ch_moderators @filter(uid($userUid)))", false},
		{"a channel's restore", GetDeletedChannelRecordingInfo, "recording_channel", "ch_is_admin: count(ch_moderators @filter(uid($userUid)))", true},
		{"a conversation's delete", GetDgraphDmRecordingInfoByEgressId, "recording_dm", "dm_is_member: count(dm_participants @filter(uid($userUid)))", false},
		{"a conversation's restore", GetDeletedDmRecordingInfo, "recording_dm", "dm_is_member: count(dm_participants @filter(uid($userUid)))", true},
	} {
		asked, vars = "", nil
		if _, err := c.read(context.Background(), "EG_1", "0x9"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", c.name, err)
		}
		switch {
		case asked == "":
			t.Errorf("%s read nothing", c.name)
		case strings.Contains(asked, live) == c.deleted:
			t.Errorf("%s reads a live recording: %v, want %v:\n%s", c.name, !c.deleted, c.deleted, asked)
		case c.deleted && !strings.Contains(asked, dgraphquery.Deleted("recording_deleted_at")):
			t.Errorf("%s doesn't read a deleted recording:\n%s", c.name, asked)
		case !strings.Contains(asked, dgraphquery.Deleted("recording_ended_at")):
			t.Errorf("%s reads a recording that hasn't ended:\n%s", c.name, asked)
		case !strings.Contains(asked, "has("+c.home+")") || !strings.Contains(asked, c.standing):
			t.Errorf("%s doesn't read it under its home with the asker's standing:\n%s", c.name, asked)
		case vars["$id"] != "EG_1" || vars["$userUid"] != "0x9" || strings.Contains(asked, "EG_1"):
			t.Errorf("%s: the ids aren't query variables: %v", c.name, vars)
		}
	}
}
