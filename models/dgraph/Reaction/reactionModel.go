package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func GetDgraphPostInfoByUID(ctx context.Context, query string, variables map[string]string) (dgraphReaction *dgraphStruct.DgraphReaction, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUID failed to get post err: %+v",
			err)
		return
	}

	type Reactions struct {
		ReactionInfo []dgraphStruct.DgraphReaction `json:"reactionInfo"`
	}

	var reactionsInfo Reactions
	err = json.Unmarshal(resp.Json, &reactionsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(reactionsInfo.ReactionInfo) == 0 {
		err = errors.New("failed to get dgraph reaction")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphPostInfoByUUID failed to get dgraph reaction")

		return
	}
	dgraphReaction = &reactionsInfo.ReactionInfo[0]

	return
}
