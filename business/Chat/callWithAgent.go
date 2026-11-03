package business

import (
	"context"
	"net/http"

	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// errCallWithAgent is the answer to a DM's call with only agents (V-126). An
// agent (a bot, by class: is_bot) takes messages, not calls. The web never
// offers one (lib/botCopy), and this is the server's word on it, for any
// client: a CallRefusal, so the call page shows its words.
var errCallWithAgent = &helpers.CallRefusal{
	Code:   "agent",
	Msg:    "You can't call an agent. Send it a message instead.",
	Status: http.StatusForbidden,
}

// RefuseCallWith answers a DM's call by who else would be in it: a refusal
// when every one of them is an agent, nil when a person is among them (and
// for nobody, which its callers answer first). Pure.
func RefuseCallWith(others ...*dgraphStruct.DgraphUser) error {
	agents := 0
	for _, o := range others {
		switch {
		case o == nil:
		case !o.IsBot:
			return nil
		default:
			agents++
		}
	}
	if agents > 0 {
		return errCallWithAgent
	}
	return nil
}

// RefuseCallIn answers a call in a DM's room, a pair's or a group's, by
// everyone else in it (CR-178's F1): POST /groupChat/getCallToken takes any
// DM's id, and a pair's is the room POST /dm/getCallToken refuses with an
// agent. A failed read is returned as its error, never as a yes.
func RefuseCallIn(ctx context.Context, callerUUID string, groupingID string) error {
	others, err := domain.GetDmCallOthers(ctx, groupingID, callerUUID)
	if err != nil {
		return err
	}
	return RefuseCallWith(others...)
}
