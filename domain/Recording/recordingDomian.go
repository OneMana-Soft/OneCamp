package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
)

func CreateOrUpdateDgraphRecording(ctx context.Context, dgraphRecording *dgraphStruct.DgraphRecording) (recordingUid string, err error) {

	dgraphRecording.DType = []string{"Recording"}
	query := fmt.Sprintf(`query {
									  recording as var(func: eq(recording_egress_id, "%+v"))
								  }`, dgraphRecording.EgressId)

	recordingUid, err = dgraphModels.CreateOrUpdateDgraphRecording(ctx, dgraphRecording, query, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphRecording Failed to create/update recording err: %+v",
			err)
		return
	}
	return

}

// recordingHome is a conversation a recording is filed under, as its delete
// and its restore read it there: the edge from the recording, and the asker's
// standing at its other end (counts, so none is 0).
type recordingHome struct{ edge, standing string }

var (
	channelHome = recordingHome{"recording_channel", `ch_uuid
						ch_is_member: count(ch_members @filter(uid($userUid)))
						ch_is_admin: count(ch_moderators @filter(uid($userUid)))`}
	dmHome = recordingHome{"recording_dm", `dm_grouping_id
						dm_is_member: count(dm_participants @filter(uid($userUid)))`}
)

// recordingInfoQuery reads a recording by its egress id in one state, state
// being dgraphquery.Live (to delete it) or Deleted (to bring it back) on
// recording_deleted_at: ended, and filed under home, with the asker's standing
// there. A recording that was deleted or archived stayed deletable, and so
// re-stamped, before the delete asked for a live one. The read takes the
// first node it finds, and a stray node an old archive or delete made holds
// the same egress id with no home, so asking for the home keeps it out. The
// ids are query variables, never query text. Pure.
func recordingInfoQuery(home recordingHome, state string) string {
	return `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id)) @filter(` + dgraphquery.Deleted("recording_ended_at") + ` AND ` + state + ` AND has(` + home.edge + `)) {
					recording_egress_id
					recording_obj_key
					` + home.edge + ` {
						` + home.standing + `
					}
				}
			}`
}

// recordingInfoRead reads one recording for its delete or its restore (a seam
// for tests).
var recordingInfoRead = dgraphModels.GetDgraphRecordingInfo

// recordingInfo reads a recording with recordingInfoQuery: ErrNotFound when
// there is none, and a read that failed logged under where.
func recordingInfo(ctx context.Context, where string, home recordingHome, state, egressId, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	rec, err := recordingInfoRead(ctx, recordingInfoQuery(home, state), map[string]string{"$id": egressId, "$userUid": userDgraphUID})
	if err != nil && !errors.Is(err, ErrNotFound) {
		helpers.LogErrorWithContext(ctx, "domain/%s Failed to get recording in dgraph err: %+v", where, err)
	}
	return rec, err
}

// GetDgraphChannelRecordingInfoByEgressId reads a channel's live recording to
// delete it, with the asker's standing in the channel (member, moderator).
func GetDgraphChannelRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	return recordingInfo(ctx, "GetDgraphChannelRecordingInfoByEgressId", channelHome, dgraphquery.Live("recording_deleted_at"), egressId, userDgraphUID)
}

// GetDeletedChannelRecordingInfo reads a channel's deleted recording to bring
// it back, as GetDgraphChannelRecordingInfoByEgressId reads a live one.
func GetDeletedChannelRecordingInfo(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	return recordingInfo(ctx, "GetDeletedChannelRecordingInfo", channelHome, dgraphquery.Deleted("recording_deleted_at"), egressId, userDgraphUID)
}

// GetDgraphDmRecordingInfoByEgressId reads a DM's or a group's live recording
// to delete it, with whether the asker is in the conversation.
func GetDgraphDmRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	return recordingInfo(ctx, "GetDgraphDmRecordingInfoByEgressId", dmHome, dgraphquery.Live("recording_deleted_at"), egressId, userDgraphUID)
}

// GetDeletedDmRecordingInfo reads a DM's or a group's deleted recording to
// bring it back, as GetDgraphDmRecordingInfoByEgressId reads a live one.
func GetDeletedDmRecordingInfo(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	return recordingInfo(ctx, "GetDeletedDmRecordingInfo", dmHome, dgraphquery.Deleted("recording_deleted_at"), egressId, userDgraphUID)
}

// EndRecording writes the end of the recording with this egress id: when it
// ended, its length, size and status. Nothing is written for an egress id no
// recording here has (its webhook was for another server's call).
func EndRecording(ctx context.Context, egressID string, fields map[string]interface{}) error {
	query := `query Recording($id: string) {
		recording as var(func: eq(recording_egress_id, $id)) @filter(has(recording_stared_at) AND has(recording_obj_key))
	}`
	return dgraphModels.SetFoundRecordingsFields(ctx, query, map[string]string{"$id": egressID}, "recording", fields)
}

// playFields is what the player needs to know about a recording: whether it
// has ended, how long it is, how it ended, and whether it is a transcript with
// no media. Not filtered by ending, so the player can say "still recording"
// rather than "not found".
const playFields = `
						recording_egress_id
						recording_obj_key
						recording_ended_at
						recording_duration
						recording_transcript_only
						recording_status`

// ErrNotFound: no recording matched (any other error means the read failed).
var ErrNotFound = dgraphModels.ErrNotFound

// GetChannelRecordingForPlay answers a channel's recording by egress id for
// the player, with the asker's membership of the channel. A recording that is
// deleted or archived is not found.
func GetChannelRecordingForPlay(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	query := `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id)) @filter(not gt(recording_deleted_at, "1970-01-01T00:00:00Z")) {` + playFields + `
						recording_channel {
							ch_uuid
							ch_is_member: count(ch_members @filter(uid($userUid)))
						}
				}
			}`
	return dgraphModels.GetDgraphRecordingInfo(ctx, query, map[string]string{"$id": egressId, "$userUid": userDgraphUID})
}

// GetDmRecordingForPlay is GetChannelRecordingForPlay for a DM's or a group's
// recording.
func GetDmRecordingForPlay(ctx context.Context, egressId string, userDgraphUID string) (*dgraphStruct.DgraphRecording, error) {
	query := `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id)) @filter(not gt(recording_deleted_at, "1970-01-01T00:00:00Z")) {` + playFields + `
						recording_dm {
							dm_grouping_id
							dm_is_member: count(dm_participants @filter(uid($userUid)))
						}
				}
			}`
	return dgraphModels.GetDgraphRecordingInfo(ctx, query, map[string]string{"$id": egressId, "$userUid": userDgraphUID})
}

// GetEventRecordingForPlay is GetChannelRecordingForPlay for a recording filed
// under a calendar event (recording_event, meet-02): whoever may read the
// event may play it, which the caller decides.
func GetEventRecordingForPlay(ctx context.Context, egressId string) (*dgraphStruct.DgraphRecording, error) {
	return getEventRecording(ctx, egressId, dgraphquery.Live("recording_deleted_at"))
}

// GetDeletedEventRecording is a recording filed under a calendar event that
// was deleted, read as GetEventRecordingForPlay reads a live one, to bring it
// back: whoever may change the event may, which the caller decides.
func GetDeletedEventRecording(ctx context.Context, egressId string) (*dgraphStruct.DgraphRecording, error) {
	return getEventRecording(ctx, egressId, dgraphquery.Deleted("recording_deleted_at"))
}

// GetEventRecordingTranscript is one page of the transcript of a live
// recording filed under a calendar event (meet-02): its lines in the order
// they were said, their count, and the recording's start, which the player
// places them against. It answers in the shape a conversation's recording
// transcript does (GetChannelRecordingTranscript), so the one player reads
// both. Whoever may read the event may read it, which the caller decides.
// The id is a query variable, never query text, and the page is two numbers;
// a page so far on that its offset overflows reads as none.
func GetEventRecordingTranscript(ctx context.Context, egressId string, page helpers.Page) (*dgraphStruct.DgraphRecording, error) {
	if page.Size < 1 || page.Offset() < 0 {
		return nil, ErrNotFound
	}
	query := fmt.Sprintf(`query RecordingTranscript($id: string){
				recordingInfo(func: eq(recording_egress_id, $id)) @filter(%s AND has(recording_event)) {
					recording_egress_id
					recording_stared_at
					recording_transcript_only
					recording_event
					recording_transcript_count: count(recording_transcript)
					recording_transcript (orderasc: transcript_timestamp, first: %d, offset: %d) {
						transcript_text
						transcript_timestamp
						transcript_offset_ms
						transcript_from {
							user_name
							user_uuid
							user_profile_object_key
						}
					}
				}
			}`, dgraphquery.Live("recording_deleted_at"), page.Size, page.Offset())
	return dgraphModels.GetDgraphRecordingInfo(ctx, query, map[string]string{"$id": egressId})
}

// getEventRecording reads a recording filed under an event in one state:
// state is dgraphquery.Live or Deleted on recording_deleted_at. The id is a
// query variable, never query text.
func getEventRecording(ctx context.Context, egressId, state string) (*dgraphStruct.DgraphRecording, error) {
	query := `query RecordingInfo($id: string){
				recordingInfo(func: eq(recording_egress_id, $id)) @filter(` + state + ` AND has(recording_event)) {` + playFields + `
						recording_event
				}
			}`
	return dgraphModels.GetDgraphRecordingInfo(ctx, query, map[string]string{"$id": egressId})
}

// GetRecordingEgressIdsOlderThan returns egress IDs of completed recordings older than cutoff.
func GetRecordingEgressIdsOlderThan(ctx context.Context, cutoff time.Time) ([]string, error) {
	cutoffStr := cutoff.Format(time.RFC3339)
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffStr
	// The block is named recordingInfo because that is the key
	// GetDgraphRecordingsList reads; under any other name the list parses as
	// empty and the policy archives nothing. See recordingListName_test.go.
	query := `query Recordings($cutoff: string){
		recordingInfo(func: has(recording_egress_id)) @filter(lt(recording_stared_at, $cutoff) AND gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z")) {
			recording_egress_id
		}
	}`

	recordings, err := dgraphModels.GetDgraphRecordingsList(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetRecordingEgressIdsOlderThan failed: %v", err)
		return nil, err
	}

	var ids []string
	for _, r := range recordings {
		if r.EgressId != "" {
			ids = append(ids, r.EgressId)
		}
	}
	return ids, nil
}

// BulkArchiveRecordings soft-deletes the recordings with these egress ids.
func BulkArchiveRecordings(ctx context.Context, egressIds []string) error {
	err := setRecordingsDeletedAt(ctx, egressIds, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkArchiveRecordings failed: %v", err)
	}
	return err
}

// BulkRestoreRecordings clears recording_deleted_at on the recordings with
// these egress ids.
func BulkRestoreRecordings(ctx context.Context, egressIds []string) error {
	return setRecordingsDeletedAt(ctx, egressIds, time.Time{})
}

// recordingBatch bounds the egress ids one upsert names.
const recordingBatch = 200

// setRecordingsDeletedAt stamps recording_deleted_at on the recordings with
// these egress ids.
//
// The mutations used to name no node, and a node in a mutation that names none
// is a new one: each archive or restore made a node holding only an egress id
// and the time, once per recording per run, and never touched the recording.
// So an archived recording was never archived, and the purge, finding those
// stray nodes, removed them and never the recordings' files. Now the upsert's
// query finds the recordings (recordingsDeletedAtQuery) and the mutation names
// what it found; an id that finds nothing changes nothing.
func setRecordingsDeletedAt(ctx context.Context, egressIds []string, at time.Time) error {
	for start := 0; start < len(egressIds); start += recordingBatch {
		query, variables := recordingsDeletedAtQuery(egressIds[start:min(start+recordingBatch, len(egressIds))])
		if err := dgraphModels.SetDgraphRecordingsDeletedAt(ctx, query, variables, "recordings", at); err != nil {
			return err
		}
	}
	return nil
}

// recordingsDeletedAtQuery is setRecordingsDeletedAt's upsert query for one
// batch of egress ids, and its variables. It finds every node a list shows
// under those ids: a recording, which has a start time from the moment it
// starts, and a call's transcript-only node, which may have none (the boot
// repair ends one with no start, MarkCallSessionNodeEnded) and which the
// lists show all the same. A stray node an old archive or delete made holds
// only an egress id and a time, with neither, and is never touched. The ids
// are query variables, never query text. Pure.
func recordingsDeletedAtQuery(batch []string) (string, map[string]string) {
	params, names, variables := egressIDVars(batch)
	return fmt.Sprintf(`query Recordings(%s) {
			recordings as var(func: eq(recording_egress_id, [%s])) @filter(has(recording_stared_at) OR eq(recording_transcript_only, true))
		}`, params, names), variables
}

// egressIDVars names egress ids as query variables, $e0, $e1 and on: their
// declarations, for the query's parameters; their names, for a list (as in
// eq(recording_egress_id, [<names>])); and their values. Pure.
func egressIDVars(ids []string) (params, names string, variables map[string]string) {
	decls := make([]string, len(ids))
	list := make([]string, len(ids))
	variables = make(map[string]string, len(ids))
	for i, id := range ids {
		list[i] = "$e" + strconv.Itoa(i)
		decls[i] = list[i] + ": string"
		variables[list[i]] = id
	}
	return strings.Join(decls, ", "), strings.Join(list, ", "), variables
}

// restorableRecording is the filter for a recording the archive keeps, which
// its list, its count and a restore's lookup share, so the list never shows
// what a restore can't bring back: a recording (it has a start time) or a
// call's transcript-only node (which may have none), ended, and filed under a
// channel, a conversation or an event. A stray node an old archive or delete
// made holds only an egress id and a time, and a recording still running has
// no end: neither is listed, and neither is restored.
var restorableRecording = `(has(recording_stared_at) OR eq(recording_transcript_only, true)) AND ` +
	dgraphquery.Deleted("recording_ended_at") +
	` AND (has(recording_channel) OR has(recording_dm) OR has(recording_event))`

// RecordingKeysHere is which of keys (egress ids, or call session keys) name a
// recording the archive keeps here (restorableRecording), deleted or live:
// what an archive restore may name. In keys' order, each once.
func RecordingKeysHere(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	query, variables := recordingKeysQuery(keys)
	recs, err := dgraphModels.GetDgraphRecordingsList(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RecordingKeysHere failed: %v", err)
		return nil, err
	}
	found := make(map[string]bool, len(recs))
	for _, r := range recs {
		found[r.EgressId] = true
	}
	here := make([]string, 0, len(found))
	for _, key := range keys {
		if found[key] {
			here = append(here, key)
			delete(found, key)
		}
	}
	return here, nil
}

// recordingKeysQuery is RecordingKeysHere's read, in one query (a restore
// names at most the archive controller's cap), and its variables: the keys
// are query variables, never query text. Pure.
func recordingKeysQuery(keys []string) (string, map[string]string) {
	params, names, variables := egressIDVars(keys)
	return fmt.Sprintf(`query Recordings(%s) {
			recordingInfo(func: eq(recording_egress_id, [%s])) @filter(%s) {
				recording_egress_id
			}
		}`, params, names, restorableRecording), variables
}

// CountRecentlyArchivedRecordings returns the number of recordings soft-deleted
// after cutoff that the archive keeps (restorableRecording).
func CountRecentlyArchivedRecordings(ctx context.Context, cutoffRFC3339 string) (int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	query := `query Recordings($cutoff: string){
		recording_count(func: has(recording_egress_id)) @filter(gt(recording_deleted_at, $cutoff) AND ` + restorableRecording + `) { count(uid) }
	}`

	list, err := dgraphModels.GetDgraphRecordingsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CountRecentlyArchivedRecordings failed: %v", err)
		return 0, err
	}
	return int64(list.Count), nil
}

// GetRecentlyArchivedRecordings returns the recordings soft-deleted after cutoff
// that the archive keeps (restorableRecording), paginated, and how many there
// are: what a restore by their keys brings back.
func GetRecentlyArchivedRecordings(ctx context.Context, cutoffRFC3339 string, first, offset int) ([]*dgraphStruct.DgraphRecording, int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	variables["$first"] = strconv.Itoa(first)
	variables["$offset"] = strconv.Itoa(offset)
	query := `query Recordings($cutoff: string, $first: int, $offset: int){
		recording_count(func: has(recording_egress_id)) @filter(gt(recording_deleted_at, $cutoff) AND ` + restorableRecording + `) { count(uid) }
		recordingInfo(func: has(recording_egress_id), first: $first, offset: $offset, orderdesc: recording_deleted_at)
			@filter(gt(recording_deleted_at, $cutoff) AND ` + restorableRecording + `) {
				uid
				recording_egress_id
				recording_deleted_at
			}
	}`

	list, err := dgraphModels.GetDgraphRecordingsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetRecentlyArchivedRecordings failed: %v", err)
		return nil, 0, err
	}
	return list.Recordings, int64(list.Count), nil
}
