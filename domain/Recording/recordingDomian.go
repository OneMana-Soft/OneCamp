package domain

import (
	"context"
	"fmt"
	"strconv"
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

// SoftDeleteDgraphRecording soft-deletes a recording by setting recording_deleted_at.
func SoftDeleteDgraphRecording(ctx context.Context, egressId string) error {
	currentTime := time.Now()
	dgraphRec := &dgraphStruct.DgraphRecording{
		DType:     []string{"Recording"},
		EgressId:  egressId,
		DeletedAt: &currentTime,
	}

	query := fmt.Sprintf(`query {
		recording as var(func: eq(recording_egress_id, "%+v"))
	}`, egressId)

	recordingUid, err := dgraphModels.CreateOrUpdateDgraphRecording(ctx, dgraphRec, query, "")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SoftDeleteDgraphRecording failed for %s: %+v", egressId, err)
		return err
	}
	_ = recordingUid
	return nil
}
func GetDgraphChannelRecordingInfoByEgressId(ctx context.Context, egressId string, userDgraphUID string) (dgraphRecording *dgraphStruct.DgraphRecording, err error) {

	variables := make(map[string]string)
	variables["$id"] = egressId
	variables["$userUid"] = userDgraphUID
	query := `query RecordingInfo($id: string, $userUid: string){
				recordingInfo(func: eq(recording_egress_id, $id))  @filter( gt(recording_ended_at, "1970-01-01T00:00:00Z")) {
					recording_egress_id
					recording_obj_key
					recording_channel {
						ch_uuid
						ch_is_member: count(ch_members @filter(uid($userUid)))
						
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
				recordingInfo(func: eq(recording_egress_id, $id))  @filter( gt(recording_ended_at, "1970-01-01T00:00:00Z")) {
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

// RestoreDgraphRecording clears recording_deleted_at (un-soft-deletes).
func RestoreDgraphRecording(ctx context.Context, egressId string) error {
	zeroTime := time.Time{}
	dgraphRec := &dgraphStruct.DgraphRecording{
		DType:     []string{"Recording"},
		EgressId:  egressId,
		DeletedAt: &zeroTime,
	}

	query := fmt.Sprintf(`query {
		recording as var(func: eq(recording_egress_id, "%+v"))
	}`, egressId)

	_, err := dgraphModels.CreateOrUpdateDgraphRecording(ctx, dgraphRec, query, "")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RestoreDgraphRecording failed for %s: %+v", egressId, err)
		return err
	}
	return nil
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

// BulkArchiveRecordings soft-deletes multiple recordings in a single Dgraph mutation.
func BulkArchiveRecordings(ctx context.Context, egressIds []string) error {
	now := time.Now()
	recordings := make([]*dgraphStruct.DgraphRecording, len(egressIds))
	for i, egressId := range egressIds {
		recordings[i] = &dgraphStruct.DgraphRecording{
			DType:     []string{"Recording"},
			EgressId:  egressId,
			DeletedAt: &now,
		}
	}

	query := `query { recordings as var(func: has(recording_egress_id)) }`
	err := dgraphModels.BulkSoftDeleteDgraphRecordings(ctx, recordings, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkArchiveRecordings failed: %v", err)
		return err
	}
	return nil
}

// BulkRestoreRecordings clears recording_deleted_at on multiple recordings.
func BulkRestoreRecordings(ctx context.Context, egressIds []string) error {
	zeroTime := time.Time{}
	recordings := make([]*dgraphStruct.DgraphRecording, len(egressIds))
	for i, egressId := range egressIds {
		recordings[i] = &dgraphStruct.DgraphRecording{
			DType:     []string{"Recording"},
			EgressId:  egressId,
			DeletedAt: &zeroTime,
		}
	}

	query := `query { recordings as var(func: has(recording_egress_id)) }`
	return dgraphModels.BulkSoftDeleteDgraphRecordings(ctx, recordings, query)
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
