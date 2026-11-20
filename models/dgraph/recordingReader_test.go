package models

import "testing"

func TestARecordingsReaderIsOnlyWhoeverIsInItsPlace(t *testing.T) {
	inCh := &DgraphRecording{Channel: &DgraphChannel{Uuid: "c1", IsMember: 1}}
	outCh := &DgraphRecording{Channel: &DgraphChannel{Uuid: "c1"}}
	inDm := &DgraphRecording{Dm: &DgraphDm{GroupingId: "g1", ParticipantIsMember: 1}}
	outDm := &DgraphRecording{Dm: &DgraphDm{GroupingId: "g1"}}
	for _, c := range []struct {
		name  string
		rec   *DgraphRecording
		place string
		want  bool
	}{
		{"no recording", nil, "", false},
		{"filed nowhere", &DgraphRecording{}, "", false},
		{"a channel's member, any place", inCh, "", true},
		{"a channel's member, through it", inCh, "c1", true},
		{"a channel's member, through another place", inCh, "c2", false},
		{"not a channel's member, any place", outCh, "", false},
		{"not a channel's member, through it", outCh, "c1", false},
		{"in a conversation, any place", inDm, "", true},
		{"in a conversation, through it", inDm, "g1", true},
		{"in a conversation, through another place", inDm, "g2", false},
		{"not in a conversation, through it", outDm, "g1", false},
		{"a channel's member, through its conversation's id", inCh, "g1", false},
	} {
		if got := c.rec.ReaderIsIn(c.place); got != c.want {
			t.Errorf("%s (place %q): %v, want %v", c.name, c.place, got, c.want)
		}
	}
}
