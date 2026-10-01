package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateOrUpdateDgraphRecording(ctx context.Context, dgraphRecording *dgraphStruct.DgraphRecording, query string, delStringJSON string) (recordingUid string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphRecording)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphRecording failed to marshal dgraphRecording struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson:    pb,
		DeleteJson: []byte(delStringJSON),
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphRecording failed to create or update dgraph recording err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	recordingUid = res.Uids["uid(recording)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateDgraphRecording failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}

func GetDgraphRecordingInfo(ctx context.Context, query string, variables map[string]string) (dgraphRecoridng *dgraphStruct.DgraphRecording, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingInfo failed to get recording err: %+v",
			err)
		return
	}

	type Recordings struct {
		RecordingInfo []dgraphStruct.DgraphRecording `json:"recordingInfo"`
	}

	var recordingsInfo Recordings
	err = json.Unmarshal(resp.Json, &recordingsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingInfo failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(recordingsInfo.RecordingInfo) == 0 {
		err = errors.New("failed to get dgraph recording")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingInfo failed to get dgraph recording")

		return
	}
	dgraphRecoridng = &recordingsInfo.RecordingInfo[0]

	return
}
func GetDgraphRecordingsList(ctx context.Context, query string, variables map[string]string) (dgraphRecordings []*dgraphStruct.DgraphRecording, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingsList failed to get recordings err: %+v",
			err)
		return
	}

	type Recordings struct {
		RecordingInfo []*dgraphStruct.DgraphRecording `json:"recordingInfo"`
	}

	var recordingsInfo Recordings
	err = json.Unmarshal(resp.Json, &recordingsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingsList failed to unmarshal response json err: %+v",
			err)
		return
	}

	dgraphRecordings = recordingsInfo.RecordingInfo

	return
}

// GetDgraphRecordingsWithCount returns recordings plus a count block in one query.
func GetDgraphRecordingsWithCount(ctx context.Context, query string, variables map[string]string) (*dgraphStruct.DgraphRecordingList, error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingsWithCount failed to get recordings err: %+v", err)
		return nil, err
	}

	type Recordings struct {
		RecordingInfo []*dgraphStruct.DgraphRecording `json:"recordingInfo"`
		Count         []map[string]int                `json:"recording_count"`
	}

	var recordingsInfo Recordings
	if err := json.Unmarshal(resp.Json, &recordingsInfo); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphRecordingsWithCount failed to unmarshal response json err: %+v", err)
		return nil, err
	}

	result := &dgraphStruct.DgraphRecordingList{
		Recordings: recordingsInfo.RecordingInfo,
	}
	if len(recordingsInfo.Count) > 0 {
		for _, v := range recordingsInfo.Count {
			for _, c := range v {
				result.Count += uint64(c)
			}
		}
	}
	return result, nil
}

// BulkSoftDeleteDgraphRecordings sets recording_deleted_at on multiple recordings in a single mutation.
func BulkSoftDeleteDgraphRecordings(ctx context.Context, recordings []*dgraphStruct.DgraphRecording, query string) error {
	txn := dgraphInit.DgraphClient.NewTxn()

	pb, err := json.Marshal(recordings)
	if err != nil {
		return err
	}

	mu := &api.Mutation{SetJson: pb}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphRecordings failed: %v", err)
		return err
	}

	return nil
}
