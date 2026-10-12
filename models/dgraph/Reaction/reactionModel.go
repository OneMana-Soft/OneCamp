package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// GetReactionOnParent runs a query whose block "on" is the parent, holding
// under "reactions" the one reaction asked for. It returns that reaction, or
// nil when the parent holds no such reaction.
func GetReactionOnParent(ctx context.Context, query string, variables map[string]string) (*dgraphStruct.DgraphReaction, error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetReactionOnParent failed to get reaction err: %+v",
			err)
		return nil, err
	}

	var found struct {
		On []struct {
			Reactions []dgraphStruct.DgraphReaction `json:"reactions"`
		} `json:"on"`
	}
	if err := json.Unmarshal(resp.Json, &found); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetReactionOnParent failed to unmarshal response json err: %+v",
			err)
		return nil, err
	}

	for _, parent := range found.On {
		if len(parent.Reactions) > 0 {
			return &parent.Reactions[0], nil
		}
	}
	return nil, nil
}
