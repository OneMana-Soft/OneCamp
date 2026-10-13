package business

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// pollState is the transient tally for an interactive poll, keyed by trigger id.
type pollState struct {
	Question string         `json:"question"`
	Options  []string       `json:"options"`
	Votes    map[string]int `json:"votes"`  // optionIndex(as string) → count
	Voters   map[string]int `json:"voters"` // userUUID → optionIndex
}

// handlePoll parses `/poll "Question?" "Option A" "Option B" ...` and returns
// an interactive Block Kit card. Voting round-trips through handlePollInteract.
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

// handlePollInteract records a vote and re-renders the card in place. The vote
// tally is updated under optimistic concurrency so simultaneous voters can't
// clobber each other's votes (naive load→mutate→save loses concurrent writes).
func handlePollInteract(ctx context.Context, cc CommandContext, ir commandAdapter.InteractRequest) (*commandAdapter.CommandResponse, error) {
	// Validate the option index up front (cheap, no state needed).
	idx, err := strconv.Atoi(strings.TrimPrefix(ir.ActionID, "vote_"))
	if err != nil || idx < 0 {
		return errorResponse("Invalid option."), nil
	}
	voter := cc.User.UserDgraphInfo.Uuid

	// Existence check so an expired poll returns a friendly message rather than
	// silently recreating empty state.
	if _, ok := loadPollState(ctx, ir.TriggerID); !ok {
		return errorResponse("This poll has expired."), nil
	}

	var invalidOption bool
	atomicErr := redisStore.UpdateJSONAtomic(
		ctx, registry.CommandInteraction, []string{ir.TriggerID}, registry.CommandInteraction.TTL,
		func(cur pollState, found bool) pollState {
			if !found {
				return cur // nothing to update; existence was checked above
			}
			if cur.Votes == nil {
				cur.Votes = map[string]int{}
			}
			if cur.Voters == nil {
				cur.Voters = map[string]int{}
			}
			if idx >= len(cur.Options) {
				invalidOption = true
				return cur
			}
			// Remove any previous vote so each user counts once.
			if prev, voted := cur.Voters[voter]; voted {
				key := strconv.Itoa(prev)
				if cur.Votes[key] > 0 {
					cur.Votes[key]--
				}
				if prev == idx {
					// Clicking the same option again retracts the vote.
					delete(cur.Voters, voter)
					return cur
				}
			}
			cur.Votes[strconv.Itoa(idx)]++
			cur.Voters[voter] = idx
			return cur
		},
	)
	if atomicErr != nil {
		return errorResponse("Couldn't record your vote. Please try again."), nil
	}

	state, ok := loadPollState(ctx, ir.TriggerID)
	if !ok {
		return errorResponse("This poll has expired."), nil
	}
	if invalidOption {
		return errorResponse("Invalid option."), nil
	}

	return &commandAdapter.CommandResponse{
		ResponseType:    "in_channel",
		Blocks:          renderPollBlocks(state),
		ReplaceOriginal: true,
		TriggerID:       ir.TriggerID,
	}, nil
}

func renderPollBlocks(s *pollState) []commandAdapter.Block {
	blocks := []commandAdapter.Block{
		{Type: "header", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "📊 " + s.Question}},
	}

	total := 0
	for _, v := range s.Votes {
		total += v
	}

	for i, opt := range s.Options {
		count := s.Votes[strconv.Itoa(i)]
		pct := 0
		if total > 0 {
			pct = count * 100 / total
		}
		bar := progressBar(pct)
		blocks = append(blocks, commandAdapter.Block{
			Type: "section",
			Text: &commandAdapter.BlockText{
				Type: "mrkdwn",
				Text: fmt.Sprintf("*%s*\n%s  %d vote(s) · %d%%", opt, bar, count, pct),
			},
		})
		blocks = append(blocks, commandAdapter.Block{
			Type: "actions",
			Elements: []commandAdapter.BlockElement{
				{
					Type:     "button",
					Text:     &commandAdapter.BlockText{Type: "plain_text", Text: "Vote"},
					ActionID: "vote_" + strconv.Itoa(i),
					Value:    strconv.Itoa(i),
				},
			},
		})
	}

	blocks = append(blocks, commandAdapter.Block{
		Type: "context",
		Text: &commandAdapter.BlockText{Type: "mrkdwn", Text: fmt.Sprintf("%d total vote(s) · click again to retract", total)},
	})
	return blocks
}

func progressBar(pct int) string {
	filled := pct / 10
	if filled > 10 {
		filled = 10
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
}

func loadPollState(ctx context.Context, triggerID string) (*pollState, bool) {
	var s pollState
	found, err := redisStore.GetJSON(ctx, registry.CommandInteraction, []string{triggerID}, &s)
	if !found || err != nil {
		return nil, false
	}
	if s.Votes == nil {
		s.Votes = map[string]int{}
	}
	if s.Voters == nil {
		s.Voters = map[string]int{}
	}
	return &s, true
}
