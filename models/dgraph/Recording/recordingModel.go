package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateOrUpdateDgraphRecording(ctx context.Context, dgraphRecording *dgraphStruct.DgraphRecording, query string, delStringJSON string) (recordingUid string, err error) {
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
	}

	res, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphRecording failed to create or update dgraph recording err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	recordingUid = res.Uids["uid(recording)"]

	return
}

// ErrNotFound: no recording matched. Any other error means the read failed.
var ErrNotFound = errors.New("failed to get dgraph recording")

func GetDgraphRecordingInfo(ctx context.Context, query string, variables map[string]string) (dgraphRecoridng *dgraphStruct.DgraphRecording, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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
		err = ErrNotFound
		return
	}
	dgraphRecoridng = &recordingsInfo.RecordingInfo[0]

	return
}
func GetDgraphRecordingsList(ctx context.Context, query string, variables map[string]string) (dgraphRecordings []*dgraphStruct.DgraphRecording, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

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

// SetDgraphRecordingsDeletedAt sets recording_deleted_at to at on the nodes
// query collects into the variable named found, in one upsert. When it
// collects none, nothing is written: uid() of an empty variable in a mutation
// would make a new node.
func SetDgraphRecordingsDeletedAt(ctx context.Context, query string, variables map[string]string, found string, at time.Time) error {
	return SetFoundRecordingsFields(ctx, query, variables, found, map[string]interface{}{"recording_deleted_at": at})
}

// SetFoundRecordingsFields writes fields onto the nodes query collects into
// the variable named found, in one upsert. When it collects none, nothing is
// written: uid() of an empty variable in a mutation would make a new node.
func SetFoundRecordingsFields(ctx context.Context, query string, variables map[string]string, found string, fields map[string]interface{}) error {
	set := map[string]interface{}{"uid": "uid(" + found + ")"}
	for k, v := range fields {
		set[k] = v
	}
	pb, err := json.Marshal(set)
	if err != nil {
		return err
	}

	req := &api.Request{
		Query: query,
		Vars:  variables,
		Mutations: []*api.Mutation{{
			SetJson: pb,
			Cond:    "@if(gt(len(" + found + "), 0))",
		}},
	}
	if _, err = dgraphInit.DoCommitNow(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetFoundRecordingsFields failed: %v", err)
		return err
	}
	return nil
}

// AddTranscriptToFound adds a transcript line to the one recording that query
// collects into the variable named found, and answers whether it did. When it
// collects none, nothing is written: uid() of an empty variable in a mutation
// would make a new node.
func AddTranscriptToFound(ctx context.Context, query string, variables map[string]string, found string, line *dgraphStruct.DgraphTranscript) (bool, error) {
	named := *line
	named.Uid = "_:line"
	pb, err := json.Marshal(map[string]interface{}{
		"uid":                  "uid(" + found + ")",
		"recording_transcript": []*dgraphStruct.DgraphTranscript{&named},
	})
	if err != nil {
		return false, err
	}
	resp, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: query,
		Vars:  variables,
		Mutations: []*api.Mutation{{
			SetJson: pb,
			Cond:    "@if(eq(len(" + found + "), 1))",
		}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AddTranscriptToFound failed: %v", err)
		return false, err
	}
	_, added := resp.GetUids()["line"]
	return added, nil
}

// QueryRecordingsJSON runs a read-only recordings query and answers its JSON,
// for reads whose shape is the caller's own.
func QueryRecordingsJSON(ctx context.Context, query string, variables map[string]string) ([]byte, error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/QueryRecordingsJSON failed: %v", err)
		return nil, err
	}
	return resp.Json, nil
}
