package business

// Polls in a DM or a group chat (MC-BUG-3).
//
// /poll there answered with a card only its author saw, whose votes were kept
// in Redis for fifteen minutes, while "+ › Create a poll" offered polls in
// every composer. A conversation's poll is now the durable poll a channel's
// is: stored the same way, with no channel (polls.channel_id is the nil uuid,
// which no channel has), and posted as a message there through the composer's
// own rules (business/Send). Who may read it, vote and see the results is
// whoever takes part in the conversation its message is in, now: someone who
// leaves a group stops seeing it.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	pollModel "github.com/akashc777/OneCamp/models/postgres/Poll"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// inConversation says a poll is a DM's or a group chat's.
func inConversation(p *pollModel.Poll) bool { return p.ChannelID == uuid.Nil }

// createInConversation stores in (already normalized) as a poll in the
// conversation in.GroupingID, and posts it there as user.
func createInConversation(ctx context.Context, user *userModels.UserInfo, in NewPoll) (*View, error) {
	grp := strings.TrimSpace(in.GroupingID)
	if !takesPartIn(ctx, grp, user) {
		return nil, ErrNoAccess
	}
	p := &pollModel.Poll{
		ID: uuid.New(), ChannelID: uuid.Nil, Question: in.Question, Multiple: in.Multiple,
		CreatedBy: user.UserPostgresInfo.Id,
	}
	for i, o := range in.Options {
		p.Options = append(p.Options, pollModel.Option{ID: strconv.Itoa(i + 1), Text: o})
	}
	if in.OpenHours > 0 {
		t := time.Now().Add(time.Duration(in.OpenHours) * time.Hour)
		p.ClosesAt = &t
	}
	if err := pollModel.Create(ctx, p); err != nil {
		return nil, fmt.Errorf("store poll: %w", err)
	}
	body := PollHTML(p.ID, p.Question)
	if in.AIGenerated {
		body = `<div data-ai-generated="true">` + body + `</div>`
	}
	messageID, err := postInConversation(ctx, user, grp, body)
	if err != nil {
		_ = pollModel.Delete(ctx, p.ID)
		return nil, err
	}
	if id, perr := uuid.Parse(messageID); perr == nil {
		linkPost(ctx, p, id)
	}
	return view(p, &pollModel.Tally{Counts: map[string]int{}}, user.UserPostgresInfo.Id, true), nil
}

// postInConversation sends body to a DM (its id is its two people's, with a
// space between) or a group chat, under the rules every message there meets,
// and says the message's id.
func postInConversation(ctx context.Context, user *userModels.UserInfo, grp, body string) (string, error) {
	in := &chatAdapter.ChatInfo{TextHtml: body}
	if to, isDM := dmOther(grp, user.UserDgraphInfo.Uuid); isDM {
		in.ToUuid = to
		m, err := sendBusiness.PrepareDirectMessage(ctx, user, in)
		if err != nil {
			return "", err
		}
		out, err := m.Commit(ctx)
		if err != nil || out == nil {
			return "", fmt.Errorf("post poll: %w", err)
		}
		return out.Uuid, nil
	}
	in.GrpUuid = grp
	g, err := sendBusiness.PrepareGroupMessage(ctx, user, in)
	if err != nil {
		return "", err
	}
	out, err := g.Commit(ctx)
	if err != nil || out == nil {
		return "", fmt.Errorf("post poll: %w", err)
	}
	return out.Uuid, nil
}

// conversationOf is the conversation a DM's or group chat's poll is in, when
// user takes part in it now; ErrNoAccess otherwise, as for a channel's poll
// someone isn't in. Read through the poll's message, which is in it, and moves
// with it when a group takes someone in.
func conversationOf(ctx context.Context, p *pollModel.Poll, user *userModels.UserInfo) (string, error) {
	if p.PostID == nil {
		return "", ErrNoAccess
	}
	chat, err := chatBusiness.GetDgraphChatBasicByUUID(ctx, p.PostID.String(), user.UserDgraphInfo.Uid)
	if err != nil {
		return "", err
	}
	if chat == nil || chat.DM == nil || chat.DM.GroupingId == "" || chat.DM.ParticipantIsMember == 0 || helpers.IsSoftDeleted(chat.DeletedAt) {
		return "", ErrNoAccess
	}
	return chat.DM.GroupingId, nil
}

// dmOther is who a DM is with, for one of its two people; isDM is false for a
// group chat, whose id is one word. A DM with oneself is with oneself. Pure.
func dmOther(grp, self string) (to string, isDM bool) {
	people := strings.Fields(grp)
	if len(people) != 2 {
		return "", false
	}
	if people[0] == self {
		return people[1], true
	}
	return people[0], true
}

// takesPartIn says whether user may post in the conversation grp: one of a
// DM's two people (its id names them, and it has no node of its own until
// its first message), or a member of a group chat.
func takesPartIn(ctx context.Context, grp string, user *userModels.UserInfo) bool {
	if people := strings.Fields(grp); len(people) == 2 {
		return people[0] == user.UserDgraphInfo.Uuid || people[1] == user.UserDgraphInfo.Uuid
	}
	ok, err := chatBusiness.ParticipatesInGrouping(ctx, grp, user.UserDgraphInfo.Uid)
	return err == nil && ok
}
