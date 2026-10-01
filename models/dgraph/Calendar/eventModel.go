package Event

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateOrUpdateDgraphEvent(ctx context.Context, dgraphEvent *dgraphStruct.DgraphEvent, query string, delStringJSON string) (eventUid string, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphEvent)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateOrUpdateDgraphEvent failed to marshal dgraphEvent struct err: %+v", err)
		return
	}

	var mutations []*api.Mutation
	if len(delStringJSON) > 0 {
		muDelete := &api.Mutation{
			DeleteJson: []byte(delStringJSON),
		}
		mutations = append(mutations, muDelete)
	}

	muSet := &api.Mutation{
		SetJson: pb,
	}
	mutations = append(mutations, muSet)

	req := &api.Request{
		Query:     query,
		Mutations: mutations,
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateOrUpdateDgraphEvent failed to create or update dgraph event err: %+v", err)
		return
	}

	eventUid = res.Uids["uid(event)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/CreateOrUpdateDgraphEvent failed to discard dgraph txn err: %+v", err)
		}
	}()
	return
}

func GetDgraphEventInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphEvent *dgraphStruct.DgraphEvent, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphEventInfoByUUID failed to get event err: %+v", err)
		return
	}

	type Events struct {
		EventInfo []dgraphStruct.DgraphEvent `json:"eventInfo"`
	}

	var eventsInfo Events
	err = json.Unmarshal(resp.Json, &eventsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphEventInfoByUUID failed to unmarshal response json err: %+v", err)
		return
	}

	if len(eventsInfo.EventInfo) == 0 {
		err = errors.New("failed to get dgraph event")
		helpers.LogErrorWithContext(ctx, "models/GetDgraphEventInfoByUUID failed to get dgraph event")
		return
	}
	dgraphEvent = &eventsInfo.EventInfo[0]

	return
}

func GetDgraphUserEvents(ctx context.Context, query string, variables map[string]string) (dgraphEvents []*dgraphStruct.DgraphEvent, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphUserEvents failed to get user events err: %+v", err)
		return
	}

	type UserInfo struct {
		CreatedEvents []*dgraphStruct.DgraphEvent `json:"user_events"`
		JoinedEvents  []*dgraphStruct.DgraphEvent `json:"joined_events"`
	}
	type Users struct {
		UserInfo []UserInfo `json:"userInfo"`
	}

	var usersInfo Users
	err = json.Unmarshal(resp.Json, &usersInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphUserEvents failed to unmarshal response json err: %+v", err)
		return
	}

	dgraphEvents = []*dgraphStruct.DgraphEvent{}
	if len(usersInfo.UserInfo) > 0 {
		if len(usersInfo.UserInfo[0].CreatedEvents) > 0 {
			dgraphEvents = append(dgraphEvents, usersInfo.UserInfo[0].CreatedEvents...)
		}
		if len(usersInfo.UserInfo[0].JoinedEvents) > 0 {
			// Deduplicate in case creator is also in participants
			seen := make(map[string]bool)
			for _, e := range dgraphEvents {
				seen[e.Uuid] = true
			}
			for _, e := range usersInfo.UserInfo[0].JoinedEvents {
				if !seen[e.Uuid] {
					dgraphEvents = append(dgraphEvents, e)
					seen[e.Uuid] = true
				}
			}
		}
	}

	return
}
