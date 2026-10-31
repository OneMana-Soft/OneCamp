// Package models (Team) runs the team queries domain/Team writes, through
// models/dgexec.
package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// CreateOrUpdateDgraphTeam upserts dgraphTeam over query (whose variable is
// team) and returns the new node's uid, "" when it updated one. An aborted
// write is tried again (it failed outright before).
func CreateOrUpdateDgraphTeam(ctx context.Context, dgraphTeam *dgraphStruct.DgraphTeam, query string) (userUid string, err error) {
	uids, err := dgexec.Upsert(ctx, query, nil, dgraphTeam)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateOrUpdateDgraphTeam failed to create or update dgraph team err: %+v", err)
		return "", err
	}
	// will get uid only when new node is created
	return uids[dgexec.Var("team")], nil
}

// ErrNotFound is a team read that found nothing, which errors.Is also matches
// to helpers.ErrNotFound, as the project model's is.
var ErrNotFound = helpers.NotFound("failed to get dgraph team")

// GetDgraphTeamInfoByUUID is the first node of query's teamInfo block,
// ErrNotFound when there is none.
func GetDgraphTeamInfoByUUID(ctx context.Context, query string, variables map[string]string) (team *dgraphStruct.DgraphTeam, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &team, &err)
	team, err = dgexec.First[dgraphStruct.DgraphTeam](ctx, query, variables, "teamInfo")
	if errors.Is(err, dgexec.ErrNotFound) {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphTeamInfoByUUID failed to get dgraph team variables: %+v", variables)
		return nil, ErrNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphTeamInfoByUUID failed to get team err: %+v", err)
		return nil, err
	}
	return team, nil
}

// GetDgraphTeamList is the nodes of query's teamInfo block.
func GetDgraphTeamList(ctx context.Context, query string, variables map[string]string) (teams []*dgraphStruct.DgraphTeam, err error) {
	// Only the tasks the reader may see (models/taskread).
	defer taskread.Settle(ctx, &teams, &err)
	teams, err = dgexec.List[*dgraphStruct.DgraphTeam](ctx, query, variables, "teamInfo")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetDgraphTeamList failed to get team err: %+v", err)
	}
	return teams, err
}

// DeleteTeamEdge deletes what delStringJSON names. It ran on
// context.Background(), so a cancelled request still wrote.
func DeleteTeamEdge(ctx context.Context, delStringJSON string) error {
	_, err := dgexec.Mutate(ctx, "", nil, dgexec.Mutation{Delete: json.RawMessage(delStringJSON)})
	return err
}
