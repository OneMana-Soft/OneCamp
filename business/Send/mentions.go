package business

// Who a message tells as mentioned (chat P3, design/chat-vs-slack/PROGRAM.md
// B5): the people its chips name, and the people its broadcasts and team
// mentions reach where it is written (business/Reach). Sending asks it, and so
// do a reply in a thread and every edit, so the rules have one copy.

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Where is where a message is written, for who it reaches beyond the people
// it names.
type Where struct {
	// Channel is the channel, as read for the writer; nil outside channels.
	Channel *dgraphStruct.DgraphChannel
	// Thread is whether it is a reply in one of the conversation's threads,
	// where a broadcast reaches nobody (TN-71).
	Thread bool
	// People are a DM's or a group's people, by user uuid, when the caller
	// has them; else Conversation names it (its grouping id) and they are read
	// when a team is mentioned.
	People       []string
	Conversation string
	// Before is a channel message's words before an edit; "" for a new one.
	Before string
}

// Seams for tests.
var (
	readChannel = func(ctx context.Context, id uuid.UUID, userUID string) (*dgraphStruct.DgraphChannel, error) {
		return channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, id, userUID)
	}
	readConversation = func(ctx context.Context, userUID, groupingID string) (*dgraphStruct.DgraphDm, error) {
		return chatBusiness.GetDgraphDmBasicInfoFromDgraph(ctx, userUID, groupingID)
	}
	reachInChannel = reachBusiness.InChannel
	reachInThread  = reachBusiness.InThread
	reachAmong     = reachBusiness.AmongPeople
	checkBroadcast = reachBusiness.Check
	namedIn        = resolveMentions
)

// Mentions is everyone a message tells as mentioned: the people its chips
// name, each a registered person (else a 400), and the people its broadcasts
// and team mentions reach where it is written, each once (business/Reach). A
// broadcast the rules refuse is a 403 in the rule's words, asked only of the
// broadcasts this message adds: an edit that keeps one is never refused for
// it. A graph or database that didn't answer is a fault, not a rule.
//
// html is rewritten in place so that each person's chip says who it notifies:
// the graph's uuid and name for the uid it names, never the sender's words
// (MC-91, MC-BUG-7), on sends and edits alike.
func Mentions(ctx context.Context, user *userModels.UserInfo, where Where, html *string) ([]*dgraphStruct.DgraphUser, error) {
	named, err := namedIn(ctx, *html)
	if err != nil {
		return nil, err
	}
	*html = normaliseChips(*html, named)
	reached, err := reachedBy(ctx, user, where, *html)
	if err != nil {
		return nil, err
	}
	return reachBusiness.Merge(named, reached), nil
}

// reachedBy is who html's broadcasts and team mentions reach where it is
// written, never its writer or a bot.
func reachedBy(ctx context.Context, user *userModels.UserInfo, where Where, html string) ([]*dgraphStruct.DgraphUser, error) {
	asked := reachBusiness.Parse(html)
	if asked.Empty() {
		return nil, nil
	}
	var (
		people []*dgraphStruct.DgraphUser
		err    error
	)
	switch {
	case where.Channel != nil && where.Thread:
		people, err = reachInThread(ctx, where.Channel.Uuid, asked, user)
	case where.Channel != nil:
		if err := mayBroadcast(ctx, user, where, asked); err != nil {
			return nil, err
		}
		people, err = reachInChannel(ctx, where.Channel.Uuid, asked, user)
	default:
		if len(asked.Groups) == 0 {
			return nil, nil
		}
		in, rerr := peopleIn(ctx, user, where)
		if rerr != nil {
			return nil, rerr
		}
		people, err = reachAmong(ctx, in, asked, user)
	}
	if err != nil {
		return nil, fmt.Errorf("reading who the message reaches: %w", err)
	}
	return people, nil
}

// mayBroadcast is the rule for the broadcasts asked adds to the channel (those
// where.Before didn't have): a Rejection in the rule's words, or nil.
func mayBroadcast(ctx context.Context, user *userModels.UserInfo, where Where, asked reachBusiness.Asked) error {
	if where.Before != "" {
		had := reachBusiness.Parse(where.Before).Broadcasts
		asked.Broadcasts = slices.DeleteFunc(slices.Clone(asked.Broadcasts), func(b reachBusiness.Broadcast) bool {
			return slices.Contains(had, b)
		})
	}
	if len(asked.Broadcasts) == 0 {
		return nil
	}
	ch := where.Channel
	if ch.MemberCount == 0 {
		// A message's own read of its channel has neither its size nor, on
		// every route, its managers: read it as the writer sees it.
		id, err := uuid.Parse(ch.Uuid)
		if err != nil {
			return reject(http.StatusBadRequest, "Failed to get channel UUID", err)
		}
		fresh, err := readChannel(ctx, id, user.UserDgraphInfo.Uid)
		if err != nil || fresh == nil {
			return fmt.Errorf("reading the channel: %w", err)
		}
		ch = fresh
	}
	if r := checkBroadcast(ctx, user, ch, asked); r != nil {
		return &Rejection{Status: http.StatusForbidden, Msg: r.Msg}
	}
	return nil
}

// peopleIn is a DM's or a group's people by user uuid: where.People, or
// read by where.Conversation.
func peopleIn(ctx context.Context, user *userModels.UserInfo, where Where) ([]string, error) {
	if where.People != nil || where.Conversation == "" {
		return where.People, nil
	}
	dm, err := readConversation(ctx, user.UserDgraphInfo.Uid, where.Conversation)
	if err != nil {
		return nil, fmt.Errorf("reading the conversation's people: %w", err)
	}
	if dm == nil {
		return nil, nil
	}
	return uuidsOf(dm.Participants), nil
}

// uuidsOf is the user uuids of people, in order. Pure.
func uuidsOf(people []*dgraphStruct.DgraphUser) []string {
	ids := make([]string, 0, len(people))
	for _, p := range people {
		if p != nil && p.Uuid != "" {
			ids = append(ids, p.Uuid)
		}
	}
	return ids
}
