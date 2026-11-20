package business

// Who is in a DM or group now.
//
// Who hears of a direct or group message was read from people's settings for
// it (users_chat_notification) alone. Leaving a group kept the leaver's
// setting, and the start-up repair gave one back to anyone who had read the
// conversation, so whoever left went on hearing its new messages, the replies
// in the threads of what they wrote there and the reactions to it: by push,
// desktop notice, email and Activity.
//
// So each read of who hears (Members, GetLevelsInConversation, which
// business/Notify plans with) keeps only those in the conversation when they are
// told (InConversationNow), whatever settings are kept. Leave takes the
// leaver's setting away, and the start-up repair takes away the settings of
// everyone who left before that and gives none back (settingsRepair.go).

import (
	"context"
	"errors"

	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// groupPeople is a group's people as the graph has them: a seam for tests.
var groupPeople = conversationDomain.PeopleOf

// InConversationNow keeps those of ids who are in the DM or group grpID now,
// in their order: a DM's two people, as its id names them, with no read (a
// deactivated one is kept here, and the senders' own gates, Deliver's and
// Tell's, drop anyone inactive), or a group's participants as the graph has
// them, which Leave takes the leaver out of, without anyone deactivated. A
// group the graph doesn't have keeps nobody. A read that fails is its error,
// and whoever asked tells nobody.
func InConversationNow(ctx context.Context, grpID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var people []*dgraphStruct.DgraphUser
	if !helpers.IsDMGroupingID(grpID) {
		var err error
		people, err = groupPeople(ctx, grpID)
		if errors.Is(err, conversationDomain.ErrNoConversation) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return conversationDomain.InNow(grpID, people, ids), nil
}
