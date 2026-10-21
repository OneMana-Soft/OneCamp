package business

import (
	"context"
	"errors"
	"strings"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
)

// handlePoll parses `/poll "Question?" "Option A" "Option B" ...` and posts a
// stored poll (business/Poll) where it was asked. Votes go to the poll itself.
func handlePoll(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	args := splitArgs(cc.Text)
	if len(args) < 3 {
		return errorResponse("Usage: `/poll \"Question?\" \"Option 1\" \"Option 2\" [more options]`"), nil
	}
	question := args[0]
	options := args[1:]

	// A POLL IS DURABLE, wherever it is asked. The card this used to answer
	// with kept its votes in Redis for fifteen minutes (CommandInteraction's
	// TTL), after which every vote was lost and the card answered "This poll
	// has expired"; in a DM or a group chat only its author ever saw it
	// (MC-BUG-3). A poll is a stored poll now (business/Poll): its message is
	// posted where it was asked, votes persist, results update for everyone
	// live, and an agent can read them. The command itself only confirms.
	np := pollBusiness.NewPoll{Question: question, Options: options}
	switch {
	case cc.ChannelID != nil:
		np.ChannelUUID = cc.ChannelID.String()
	case cc.DmGroupID != nil && strings.TrimSpace(*cc.DmGroupID) != "":
		np.GroupingID = *cc.DmGroupID
	default:
		return errorResponse("Run /poll in a channel, a DM or a group chat."), nil
	}
	user := cc.User
	if _, err := pollBusiness.Create(ctx, &user, np); err != nil {
		var input pollBusiness.InputError
		if errors.As(err, &input) || errors.Is(err, pollBusiness.ErrNoAccess) {
			return errorResponse("%s", err.Error()), nil
		}
		if rj, ok := sendBusiness.AsRejection(err); ok {
			return errorResponse("%s", rj.Msg), nil
		}
		return errorResponse("Couldn't post the poll. Please try again."), nil
	}
	return ephemeral("Poll posted."), nil
}
