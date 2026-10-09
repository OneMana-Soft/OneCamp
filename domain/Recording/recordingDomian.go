package domain

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
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

// A recording opened by its egress id (to play it, read its transcript, or
// delete it) must be live: one that was deleted or archived stayed playable by
// link, since these reads didn't ask.
const liveRecording = `gt(recording_ended_at, "1970-01-01T00:00:00Z") AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z")`

func GetDgraphChannelRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (dgraphRecording *dgraphStruct.DgraphRecording, err error) {

	variables := make(map[string]string)
	variables["$id"] = egressId
	variables["$userUid"] = userDgraphUID
	query := `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id))  @filter(` + liveRecording + `) {
					recording_egress_id
					recording_obj_key
					recording_channel {
						ch_uuid
						ch_is_member: count(ch_members @filter(uid($userUid)))
						ch_is_admin: count(ch_moderators @filter(uid($userUid)))
					}
				}
			}`

	dgraphRecording, err = dgraphModels.GetDgraphRecordingInfo(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChannelRecordingInfoByEgressId Failed to get recording in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphDmRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (dgraphRecording *dgraphStruct.DgraphRecording, err error) {

	variables := make(map[string]string)
	variables["$id"] = egressId
	variables["$userUid"] = userDgraphUID
	query := `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id))  @filter(` + liveRecording + `) {
					recording_egress_id
					recording_obj_key
					recording_dm {
						dm_grouping_id
						dm_is_member: count(dm_participants @filter(uid($userUid)))
					}
				}
			}`

	dgraphRecording, err = dgraphModels.GetDgraphRecordingInfo(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDmRecordingInfoByEgressId Failed to get recording in dgraph err: %+v",
			err,
		)
		return

	}

	return

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
// query finds the recordings (by egress id, and a start time, which a stray
// node never has) and the mutation names what it found; an id that finds
// nothing changes nothing. The ids are query variables, never query text.
func setRecordingsDeletedAt(ctx context.Context, egressIds []string, at time.Time) error {
	for start := 0; start < len(egressIds); start += recordingBatch {
		batch := egressIds[start:min(start+recordingBatch, len(egressIds))]
		params := make([]string, len(batch))
		names := make([]string, len(batch))
		variables := make(map[string]string, len(batch))
		for i, id := range batch {
			names[i] = "$e" + strconv.Itoa(i)
			params[i] = names[i] + ": string"
			variables[names[i]] = id
		}
		query := fmt.Sprintf(`query Recordings(%s) {
			recordings as var(func: eq(recording_egress_id, [%s])) @filter(has(recording_stared_at))
		}`, strings.Join(params, ", "), strings.Join(names, ", "))
		if err := dgraphModels.SetDgraphRecordingsDeletedAt(ctx, query, variables, "recordings", at); err != nil {
			return err
		}
	}
	return nil
}

// CountRecentlyArchivedRecordings returns the number of recordings soft-deleted after cutoff.
func CountRecentlyArchivedRecordings(ctx context.Context, cutoffRFC3339 string) (int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	query := `query Recordings($cutoff: string){
		recording_count(func: has(recording_egress_id)) @filter(gt(recording_deleted_at, $cutoff)) { count(uid) }
	}`

	list, err := dgraphModels.GetDgraphRecordingsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CountRecentlyArchivedRecordings failed: %v", err)
		return 0, err
	}
	return int64(list.Count), nil
}

// GetRecentlyArchivedRecordings returns recordings soft-deleted after cutoff, paginated.
func GetRecentlyArchivedRecordings(ctx context.Context, cutoffRFC3339 string, first, offset int) ([]*dgraphStruct.DgraphRecording, int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	variables["$first"] = strconv.Itoa(first)
	variables["$offset"] = strconv.Itoa(offset)
	query := `query Recordings($cutoff: string, $first: int, $offset: int){
		recording_count(func: has(recording_egress_id)) @filter(gt(recording_deleted_at, $cutoff)) { count(uid) }
		recordingInfo(func: has(recording_egress_id), first: $first, offset: $offset, orderdesc: recording_deleted_at)
			@filter(gt(recording_deleted_at, $cutoff)) {
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

// RestoreDgraphRecording clears recording_deleted_at (un-soft-deletes).
