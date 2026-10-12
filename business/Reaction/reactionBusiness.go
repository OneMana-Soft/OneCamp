package business

import (
	"context"
	"net/http"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// On is the edge a comment, a post or a chat message holds its reactions by.
type On string

const (
	OnComment On = "comment_reactions"
	OnPost    On = "post_reactions"
	OnChat    On = "chat_reactions"
)

// Refusal is MayChange saying no, with the status and message the handler
// answers with.
type Refusal struct {
	Status int
	Msg    string
}

// reactionOnParent is the graph read MayChange decides on, a variable so the
// decision can be tested without a graph.
var reactionOnParent = domain.GetReactionOnParent

// MayChange checks the reaction a request names by its uid
// (reaction_dgraph_id) before anything changes or removes it, and returns
// the reaction as the graph holds it.
//
// That uid was trusted. Changing a reaction wrote it as the reaction's node,
// and removing one deleted whatever node it named once that node said the
// caller had added it. A member could name any node (uids are sequential,
// and many answers carry them), someone's private doc say: the change
// stamped the doc as their reaction, and the removal then wiped it. They
// could also change or remove anyone else's reaction.
//
// Now the uid must be a reaction, and nothing else, that the parent the
// request names (parentUID, read from the graph by the handler) holds by its
// reactions edge: 404 otherwise, which says nothing about what the uid is.
// And the caller must be who added it: 403. A read that fails is a fault
// (500), not a missing reaction.
func MayChange(ctx context.Context, on On, parentUID, reactionUID, callerUID string) (*dgraphStruct.DgraphReaction, *Refusal) {
	noSuchReaction := &Refusal{Status: http.StatusNotFound, Msg: "There's no such reaction."}
	if !dgraphquery.AllUIDs([]string{parentUID, reactionUID}) || strings.TrimLeft(reactionUID[2:], "0") == "" {
		return nil, noSuchReaction
	}

	reaction, err := reactionOnParent(ctx, string(on), parentUID, reactionUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/MayChange Failed to read reaction %s on %s err: %+v",
			reactionUID, parentUID, err)
		return nil, &Refusal{Status: http.StatusInternalServerError, Msg: "Couldn't check that reaction just now. Try again in a moment."}
	}
	if reaction == nil || len(reaction.DType) != 1 || reaction.DType[0] != "Reaction" {
		return nil, noSuchReaction
	}
	if callerUID == "" || reaction.AddedBy == nil || reaction.AddedBy.Uid != callerUID {
		return nil, &Refusal{Status: http.StatusForbidden, Msg: "You can only change or remove your own reaction."}
	}
	return reaction, nil
}
