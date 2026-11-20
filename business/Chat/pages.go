package business

// A DM's and a group's pages, their open, and a message's thread (H5B). Each
// page is one graph read (domain.ChatPage) whichever way it runs. A group's
// page and a thread ask whether the reader is in the conversation in that
// same read, and inConversation is the one place that answers it. A DM is
// read by a grouping made of its two people, so its reader is always in it;
// whether the other is anyone is asked only when the page comes back empty.
// Opening the latest page marks it read in one statement
// (lastseenDomain.OpenChat).

import (
	"context"
	"errors"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	lastseenDomain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	messageDomain "github.com/akashc777/OneCamp/domain/Message"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	chatDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Chat"
	"github.com/google/uuid"
)

var (
	// ErrNoConversation is a group that isn't there.
	ErrNoConversation = errors.New("chat: no such conversation")
	// ErrNotInConversation is a conversation its reader isn't in: never
	// joined, or since taken out.
	ErrNotInConversation = errors.New("chat: not in this conversation")
	// ErrNoSuchPerson is a DM with someone who isn't anyone.
	ErrNoSuchPerson = errors.New("chat: no such person")
)

// inConversation is the gate of a group's pages and of a message's thread,
// from what their own read said: a conversation that isn't there and one the
// reader isn't in are refused, and nothing else is let through.
func inConversation(found, member bool) error {
	if !found {
		return ErrNoConversation
	}
	if !member {
		return ErrNotInConversation
	}
	return nil
}

// edgeOf is the time a page in dir reads from: just past the edge's own
// message for an older or a newer page (helpers.PageEdge), the edge itself
// for a page that opens at a message; the latest page has none.
func edgeOf(dir messageDomain.Direction, edge helpers.PageEdge) time.Time {
	switch dir {
	case messageDomain.Older:
		return edge.Before()
	case messageDomain.Newer:
		return edge.After()
	}
	return edge.At
}

// GroupPage is the page of group grpID in dir from edge, for the person whose
// graph uid is member: the gate and the page in one read.
func GroupPage(ctx context.Context, member, grpID string, dir messageDomain.Direction, edge helpers.PageEdge) (chats ChatPagination, err error) {
	p, err := domain.ChatPage(ctx, grpID, dir, edgeOf(dir, edge), member)
	if err != nil {
		return chats, err
	}
	if err := inConversation(p.Found, p.IsMember); err != nil {
		return chats, err
	}
	chats.Chats, chats.HasMore = helpers.PageFrom(p.Chats, chatUUIDOf, edge, domain.CHAT_COUNT)
	return chats, nil
}

// DMPage is the page of me's DM with other in dir from edge (both user
// uuids). An empty page is the one case where "no such person" and "no
// messages yet" differ, so only then is other looked up, in every direction:
// ErrNoSuchPerson when they aren't anyone (a deactivated person is someone).
func DMPage(ctx context.Context, me, other string, dir messageDomain.Direction, edge helpers.PageEdge) (chats ChatPagination, err error) {
	grouping, them, err := dmGrouping(me, other)
	if err != nil {
		return chats, err
	}
	p, err := domain.ChatPage(ctx, grouping, dir, edgeOf(dir, edge), "")
	if err != nil {
		return chats, err
	}
	if len(p.Chats) == 0 {
		if err := someone(ctx, them); err != nil {
			return chats, err
		}
	}
	chats.Chats, chats.HasMore = helpers.PageFrom(p.Chats, chatUUIDOf, edge, domain.CHAT_COUNT)
	return chats, nil
}

// dmGrouping is the grouping of me's DM with other, other read as a uuid
// however it is spelled (upper case, braces, urn:uuid:) and written the one
// way every DM's grouping is, so a page never reads or marks a grouping no
// DM has: ErrNoSuchPerson when other isn't a uuid.
func dmGrouping(me, other string) (string, uuid.UUID, error) {
	them, err := uuid.Parse(other)
	if err != nil {
		return "", uuid.Nil, ErrNoSuchPerson
	}
	return helpers.GetGroupingId(me, them.String()), them, nil
}

// someone is nil when id is a person's, ErrNoSuchPerson when it isn't (the
// user read's not-found), or the read's failure.
func someone(ctx context.Context, id uuid.UUID) error {
	if _, err := userBusiness.GetUserByUUID(ctx, id); errors.Is(err, helpers.ErrNotFound) {
		return ErrNoSuchPerson
	} else if err != nil {
		return err
	}
	return nil
}

// OpenGroup is group grpID's latest page as who (member is their graph uid)
// opens it: gated as GroupPage, then marked read, saying how many were
// unread before. A refused open marks nothing.
func OpenGroup(ctx context.Context, who uuid.UUID, member, grpID string) (ChatPagination, error) {
	chats, err := GroupPage(ctx, member, grpID, messageDomain.Latest, helpers.PageEdge{})
	if err != nil {
		return chats, err
	}
	return opened(ctx, who, grpID, chats)
}

// OpenDM is who's DM with other at its latest page, as DMPage reads it, then
// marked read the same way. A DM with no one marks nothing.
func OpenDM(ctx context.Context, who uuid.UUID, other string) (ChatPagination, error) {
	grouping, _, err := dmGrouping(who.String(), other)
	if err != nil {
		return ChatPagination{}, err
	}
	chats, err := DMPage(ctx, who.String(), other, messageDomain.Latest, helpers.PageEdge{})
	if err != nil {
		return chats, err
	}
	return opened(ctx, who, grouping, chats)
}

// opened marks grouping read for who now, in the statement that counts what
// was unread before (chat B27): the page says it.
func opened(ctx context.Context, who uuid.UUID, grouping string, chats ChatPagination) (ChatPagination, error) {
	unread, err := lastseenDomain.OpenChat(ctx, who, grouping, time.Now())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Chat opening %s left it unmarked err: %+v", grouping, err)
		return ChatPagination{}, err
	}
	chats.Unread = &unread
	return chats, nil
}

// MemberThread is a message's thread as the person whose graph uid is member
// opens it, the gate asked in the thread's own read. A message that isn't
// there, or isn't in a conversation, is chatDgraphModels.ErrNotFound.
func MemberThread(ctx context.Context, chatUUID, member string) (*dgraphStruct.DgraphChat, error) {
	c, err := domain.GetDgraphChatByUUIDWithAllComments(ctx, chatUUID, member)
	if err != nil {
		return nil, err
	}
	if c == nil || c.DM == nil {
		return nil, chatDgraphModels.ErrNotFound
	}
	if err := inConversation(true, c.DM.ParticipantIsMember > 0); err != nil {
		return nil, err
	}
	return c, nil
}
