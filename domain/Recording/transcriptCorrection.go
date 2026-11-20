package domain

// The graph's side of a transcript line's correction and of an export by the
// recording's key (zoom-05): the query text, run through dgexec. A line has
// no edge back to its recording, so both find the recording by its key and
// the line under it: a uid that isn't a line of that recording (a message,
// the recording itself, another call's line) is never under the edge, which
// is the line's type and parent check. The asker's standing in the
// recording's conversation is read in the same query, by the fragments its
// delete reads (channelHome, dmHome), so the rule's facts are one read.

import (
	"context"
	"strconv"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// readerLineFields are a line's facts as an export and a correction show
// them: transcriptLineFields, and its correction. Never its kept
// translations: only the export's read (readerQuery) takes those, for a
// ?lang= read; a correction neither reads nor answers them.
const readerLineFields = transcriptLineFields + `
				transcript_original_text
				transcript_corrected_at
				transcript_corrected_by { user_uuid user_full_name }`

// readerHome is what a correction and an export take from the home: what a
// search entry takes (transcriptHomeFields' facts), the event it may belong
// to, its line count, and the asker's standing where it was made, by the
// fragments its delete reads.
func readerHome() string {
	return `
			uid
			recording_egress_id
			recording_stared_at
			recording_obj_key
			recording_deleted_at
			recording_event
			recording_transcript_count: count(recording_transcript)
			recording_channel {
				ch_name
				` + channelHome.standing + `
			}
			recording_dm {
				` + dmHome.standing + `
			}`
}

// homesInAskersPlace is the live started homes filed under $key in a
// channel or a conversation the asker ($userUid) is in, with lines (what each
// read takes of them): a var block binds the channels (inCh) and the
// conversations (inDm) of those homes that have the asker among their members
// or participants, and the homes are only those filed under one. Anyone else's
// read binds no home and no line, in the same round trip: who may not read a
// transcript is refused before any of it is read (V-1126 FIX).
func homesInAskersPlace(lines string) string {
	started := `has(recording_stared_at) AND ` + dgraphquery.Live("recording_deleted_at")
	return `
		var(func: eq(recording_egress_id, $key)) @filter(` + started + `) {
			recording_channel @filter(uid_in(ch_members, $userUid)) { inCh as uid }
			recording_dm @filter(uid_in(dm_participants, $userUid)) { inDm as uid }
		}
		homes(func: eq(recording_egress_id, $key)) @filter(` + started + ` AND (uid_in(recording_channel, uid(inCh)) OR uid_in(recording_dm, uid(inDm)))) {` +
		readerHome() + lines + `
		}`
}

// correctionQuery reads the homes in the asker's ($userUid) place filed under
// $key (homesInAskersPlace) with only the line $line under them. The ids are
// query variables, never query text.
func correctionQuery() string {
	return `query Line($key: string, $line: string, $userUid: string) {` + homesInAskersPlace(`
			recording_transcript @filter(uid($line)) {`+readerLineFields+`
			}`) + `
	}`
}

// readerQuery reads the homes in the asker's place filed under $key with
// their first $first lines in the order they were said.
func readerQuery() string {
	return `query Transcript($key: string, $userUid: string, $first: int) {` + homesInAskersPlace(`
			recording_transcript (orderasc: transcript_timestamp, first: $first) {`+readerLineFields+`
				transcript_translations
			}`) + `
	}`
}

type readerHomes struct {
	Homes []*dgraphStruct.DgraphRecording `json:"homes"`
}

// placedHome is the home of homes that is filed somewhere (a channel, a chat
// or an event), the one holding a line first: a stray node an old archive
// left holds the same key with no home. Nil when none is.
func placedHome(homes []*dgraphStruct.DgraphRecording) *dgraphStruct.DgraphRecording {
	var placed *dgraphStruct.DgraphRecording
	for _, h := range homes {
		if h == nil || (h.Channel == nil && h.Dm == nil && h.Event == "") {
			continue
		}
		if len(h.Transcript) > 0 {
			return h
		}
		if placed == nil {
			placed = h
		}
	}
	return placed
}

// CorrectTranscriptLine reads, in one transaction, the live started home
// filed under key with the asker's standing in its conversation and only the
// line lineUID under it, and hands it to decide: home is nil when no live
// started home has that key, and its Transcript is empty when the line isn't
// under it. When decide answers a line to set, it is written and committed in
// that transaction; an error from decide writes nothing and is returned as
// it is. A concurrent correction of the same line aborts one commit, and
// dgexec.InTxn runs the read and decide again, so decide sees its words.
func CorrectTranscriptLine(ctx context.Context, key, lineUID, userUID string,
	decide func(home *dgraphStruct.DgraphRecording) (*dgraphStruct.DgraphTranscript, error)) error {
	q := correctionQuery()
	return dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
		got, err := dgexec.ReadIn[readerHomes](tx, q, map[string]string{"$key": key, "$line": lineUID, "$userUid": userUID})
		if err != nil {
			return err
		}
		set, err := decide(placedHome(got.Homes))
		if err != nil || set == nil {
			return err
		}
		_, err = tx.Mutate("", nil, dgexec.Mutation{Set: set})
		return err
	})
}

// ReaderTranscript is the live started home filed under key with the asker's
// standing in its conversation and its first max lines in the order they
// were said; nil when there is none.
func ReaderTranscript(ctx context.Context, key, userUID string, max int) (*dgraphStruct.DgraphRecording, error) {
	got, err := dgexec.Read[readerHomes](ctx, readerQuery(), map[string]string{"$key": key, "$userUid": userUID, "$first": strconv.Itoa(max)})
	if err != nil {
		return nil, err
	}
	return placedHome(got.Homes), nil
}
