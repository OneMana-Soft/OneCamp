package controllers

import (
	"net/http"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CALL-BUG-15. A DM's recording opened through a channel's route (or the
// reverse) dereferenced a nil channel and the request died with a 500. It is
// a 404 now. V-1126: so is one the asker can't see (not in its channel or
// conversation, or another place's), the answer a missing recording gets, so
// nobody outside a place learns whether a recording id exists.
func TestARecordingOpenedByTheWrongRouteIsNotFound(t *testing.T) {
	dm := &dgraphStruct.DgraphRecording{Dm: &dgraphStruct.DgraphDm{GroupingId: "g1", ParticipantIsMember: 1}}
	ch := &dgraphStruct.DgraphRecording{Channel: &dgraphStruct.DgraphChannel{Uuid: "c1", IsMember: 1}}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"a DM's recording by the channel route", channelRecordingAccess(dm, "c1"), http.StatusNotFound},
		{"a channel's recording by the DM route", chatRecordingAccess(ch, "g1"), http.StatusNotFound},
		{"none, by the channel route", channelRecordingAccess(nil, "c1"), http.StatusNotFound},
		{"none, by the DM route", chatRecordingAccess(nil, "g1"), http.StatusNotFound},
		{"another channel's", channelRecordingAccess(ch, "c2"), http.StatusNotFound},
		{"not a member", channelRecordingAccess(&dgraphStruct.DgraphRecording{Channel: &dgraphStruct.DgraphChannel{Uuid: "c1"}}, "c1"), http.StatusNotFound},
		{"another conversation's", chatRecordingAccess(dm, "g2"), http.StatusNotFound},
		{"not in the conversation", chatRecordingAccess(&dgraphStruct.DgraphRecording{Dm: &dgraphStruct.DgraphDm{GroupingId: "g1"}}, "g1"), http.StatusNotFound},
		{"the channel's own", channelRecordingAccess(ch, "c1"), 0},
		{"the DM's own", chatRecordingAccess(dm, "g1"), 0},
	} {
		if c.got != c.want {
			t.Errorf("%s: %d, want %d", c.name, c.got, c.want)
		}
	}
}
