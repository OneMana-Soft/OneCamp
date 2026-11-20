package domain

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

// ConversationOf is the channel or chat a recording was made in, and its
// kind: "channel", "group" (a group chat) or "dm". "" for a home linked to
// neither (an event's call, whose readers are the calendar's).
func ConversationOf(home *dgraphStruct.DgraphRecording) (id, kind string) {
	switch {
	case home == nil:
	case home.Channel != nil && home.Channel.Uuid != "":
		return home.Channel.Uuid, "channel"
	case home.Dm != nil && home.Dm.GroupingId != "":
		if helpers.IsGroupID(home.Dm.GroupingId) {
			return home.Dm.GroupingId, "group"
		}
		return home.Dm.GroupingId, "dm"
	}
	return "", ""
}

// TranscriptHome is what a page of transcript hits is checked against: the
// recording (or call session) a line lives under, as the graph has it now.
type TranscriptHome struct {
	Deleted, Ended bool
	ConvID         string
	Lines          map[string]bool
}

// TranscriptHomes reads, in one query, the homes (by recording egress id or
// call session key) of a page of transcript hits: whether each is deleted or
// ended, the conversation it is in, and which of lineUIDs it still holds. A
// key the graph doesn't hold is missing from the answer; a line uid that
// isn't one is held by no home. The keys are query variables; the line uids
// are checked to be graph uids before they are written into the query.
// homesQuery is TranscriptHomes' read of the graph; a seam for tests.
var homesQuery = dgraphModels.QueryRecordingsJSON

func TranscriptHomes(ctx context.Context, keys, lineUIDs []string) (map[string]TranscriptHome, error) {
	keys = level.Unique(keys)
	out := make(map[string]TranscriptHome, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	params, names, variables := egressIDVars(keys)
	lines := ""
	if uids := graphUIDs(lineUIDs); len(uids) > 0 {
		lines = "recording_transcript @filter(uid(" + strings.Join(uids, ", ") + ")) { uid }"
	}
	query := `query Homes(` + params + `) {
		recordingInfo(func: eq(recording_egress_id, [` + names + `])) @filter(has(recording_stared_at) OR eq(recording_transcript_only, true)) {
			recording_egress_id
			recording_deleted_at
			recording_ended_at
			recording_channel { ch_uuid }
			recording_dm { dm_grouping_id }
			` + lines + `
		}
	}`
	raw, err := homesQuery(ctx, query, variables)
	if err != nil {
		return nil, err
	}
	var resp struct {
		RecordingInfo []*dgraphStruct.DgraphRecording `json:"recordingInfo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	for _, r := range resp.RecordingInfo {
		if r == nil || r.EgressId == "" {
			continue
		}
		id, _ := ConversationOf(r)
		home, again := out[r.EgressId]
		if !again {
			home = TranscriptHome{Ended: true, ConvID: id, Lines: map[string]bool{}}
		} else if home.ConvID != id {
			// Two nodes under one key that disagree on the conversation
			// match no hit's conversation: the stricter answer.
			home.ConvID = ""
		}
		// Two nodes under one key: deleted if either is, ended if both are.
		home.Deleted = home.Deleted || after(r.DeletedAt)
		home.Ended = home.Ended && after(r.EndedAt)
		for _, l := range r.Transcript {
			if l != nil && l.Uid != "" {
				home.Lines[l.Uid] = true
			}
		}
		out[r.EgressId] = home
	}
	return out, nil
}

// CallPlaces are the conversations a person (by uuid) is in now, read from
// the graph, that hold a call with a transcript: the uuid of each live
// channel they are a member of and the grouping id of each chat they are in.
// Through dgraphModels.MemberRecordingsBlock, the recordings list's own rule.
func CallPlaces(ctx context.Context, userUUID string) ([]string, error) {
	if userUUID == "" {
		return nil, nil
	}
	query := `query Places($uuid: string) {
				` + dgraphModels.MemberRecordingsBlock(dgraphquery.PersonByUUID("$uuid"), "has(recording_transcript)") + `
				var(func: uid(ch_recs)) { recording_channel { c as uid } }
				var(func: uid(dm_recs)) { recording_dm { d as uid } }
				chs(func: uid(c)) @filter(` + dgraphquery.Live("ch_deleted_at") + `) { ch_uuid }
				dms(func: uid(d)) { dm_grouping_id }
			}`
	raw, err := dgraphModels.QueryRecordingsJSON(ctx, query, map[string]string{"$uuid": userUUID})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Chs []struct {
			Uuid string `json:"ch_uuid"`
		} `json:"chs"`
		Dms []struct {
			GroupingID string `json:"dm_grouping_id"`
		} `json:"dms"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	places := make([]string, 0, len(resp.Chs)+len(resp.Dms))
	for _, c := range resp.Chs {
		places = append(places, c.Uuid)
	}
	for _, d := range resp.Dms {
		places = append(places, d.GroupingID)
	}
	return level.Unique(places), nil
}

// after is a time the graph has set (a live node's deletion time is the zero
// time, never nil alone), by the graph's one set-time rule.
func after(t *time.Time) bool { return helpers.IsSoftDeleted(t) }

// graphUIDs are the ids, of those given, that are graph uids, each once.
func graphUIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range level.Unique(ids) {
		if dgraphquery.AllUIDs([]string{id}) {
			out = append(out, id)
		}
	}
	return out
}
