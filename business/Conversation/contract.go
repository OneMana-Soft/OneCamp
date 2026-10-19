// Package business (Conversation) is the server half of the chat programme's
// Conversation model (design/chat-vs-slack/PROGRAM.md §4.2 #22; A9 builds it):
// a channel, a DM and a group are one Ref, with one access rule (Resolve,
// through the Access Resolver's chat kinds in access.go), one link rule
// (Route) and one page read (Pages).
//
// DAY 0. Everything here answers with today's code: the membership checks the
// handlers make (access.go), helpers.ChatPath and the channel path for links,
// and today's page queries behind Pages. Nothing calls Pages yet, so nothing
// changes by its existing. A9 replaces the defaults behind the same names
// (stable group ids, the conversations tables); A1's web model reads the same
// kinds.
package business

import (
	"context"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The chat kinds of resource, as the Access Resolver, Resource Events and the
// web app's ConversationRef name them. Never renamed once shipped.
const (
	KindChannel = "channel" // its uuid
	KindDM      = "dm"      // its grouping id: both people's uuids, sorted, a space between
	KindGroup   = "group"   // its grouping id (today a hash of its people; A9 makes it stable)
	KindThread  = "thread"  // its root message's uuid, a post's or a DM's or group's message
	KindCall    = "call"    // its LiveKit room: a channel uuid or a grouping id (helpers.ClassifyRoom)
)

// The chat levels are the Access Resolver's, named for chat: none < read
// (view) < reply in threads (comment) < post (edit) < moderate (full). A member
// of an announcement channel replies in threads and doesn't post; a thread
// decides at most reply, or moderate for a channel's managers.
const (
	LevelRead     = accessBusiness.LevelView
	LevelReply    = accessBusiness.LevelComment
	LevelPost     = accessBusiness.LevelEdit
	LevelModerate = accessBusiness.LevelFull
)

// Ref names one conversation, or a thread or call in one.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Resource is the Access Resolver's name for the conversation.
func (r Ref) Resource() accessBusiness.Resource {
	return accessBusiness.Resource{Kind: r.Kind, UUID: r.ID}
}

// Resolve is who's decision on ref, asked of the Access Resolver (the chat
// kinds of access.go answer it): ErrNotFound when ref isn't there or who can't
// read it, which answer alike. The levels say the rest: LevelPost to write in
// it, LevelReply to answer in its threads, LevelModerate to manage it.
func Resolve(ctx context.Context, who *userModels.UserInfo, ref Ref) (accessBusiness.Decision, error) {
	if who == nil {
		return accessBusiness.Decision{}, ErrNotFound
	}
	subject := accessBusiness.SubjectOf(who)
	d, err := accessBusiness.Rules.Resolve(ctx, subject, ref.Resource())
	switch {
	case errors.Is(err, accessBusiness.ErrNotFound), errors.Is(err, accessBusiness.ErrUnknownKind):
		return accessBusiness.Decision{}, ErrNotFound
	case err != nil:
		return accessBusiness.Decision{}, err
	case d.Level < LevelRead:
		return accessBusiness.Decision{}, ErrNotFound
	}
	return d, nil
}

// Route is where the conversation opens in the web app for viewerUUID: the one
// link rule every link the server writes uses (email, push, the AI's
// citations, the unread summary). With messageUUID it opens that message with
// its thread. "" for a kind with no page of its own (a thread or call: route
// its conversation with the root message). The web app's copy is
// lib/routes/conversationRoute.ts.
func (r Ref) Route(viewerUUID, messageUUID string) string {
	switch r.Kind {
	case KindChannel:
		if r.ID == "" {
			return ""
		}
		path := "/app/channel/" + r.ID
		if messageUUID != "" {
			path += "/" + messageUUID
		}
		return path
	case KindDM, KindGroup:
		return helpers.ChatPath(r.ID, viewerUUID, messageUUID)
	}
	return ""
}

// ChatRef is the DM or group a grouping id names: a DM's is its two people's
// ids with a space between (helpers.GetGroupingId), a group's has none.
func ChatRef(groupingID string) Ref {
	if strings.Contains(groupingID, " ") {
		return Ref{Kind: KindDM, ID: groupingID}
	}
	return Ref{Kind: KindGroup, ID: groupingID}
}

// Other is, for a DM, the person beside me (me, for a DM with oneself), or ""
// when me isn't one of its two people or it isn't a DM.
func (r Ref) Other(me string) string {
	ids := strings.Fields(r.ID)
	if r.Kind != KindDM || len(ids) != 2 {
		return ""
	}
	switch {
	case strings.EqualFold(ids[0], me):
		return ids[1]
	case strings.EqualFold(ids[1], me):
		return ids[0]
	}
	return ""
}

// Page is one page of a conversation, newest first as today's queries answer
// it: a channel's posts, or a DM's or group's messages.
type Page struct {
	Posts   []*dgraphStruct.DgraphPost `json:"posts,omitempty"`
	Chats   []*dgraphStruct.DgraphChat `json:"chats,omitempty"`
	HasMore bool                       `json:"has_more"`
}

// Pager reads a conversation by pages.
type Pager interface {
	// Page is the latest page of ref (before nil), or the page older than the
	// message at before, for who; ErrNotFound when who can't read it.
	Page(ctx context.Context, who *userModels.UserInfo, ref Ref, before *helpers.PageEdge) (Page, error)
}

// ErrNotFound is a conversation that isn't there or that who can't read: the
// two answer alike.
var ErrNotFound = helpers.NotFound("conversation: no such conversation")

// Pages is the page read everything asks.
var Pages Pager = todaysPages{}

// todaysPages are today's page queries, behind today's read rule (the
// latest-page GETs of /po, /dm and /groupChat). As today, a DM's or group's
// latest page marks it read for who.
type todaysPages struct{}

func (todaysPages) Page(ctx context.Context, who *userModels.UserInfo, ref Ref, before *helpers.PageEdge) (Page, error) {
	if _, err := Resolve(ctx, who, ref); err != nil {
		return Page{}, err
	}
	me := who.UserDgraphInfo.Uuid
	var err error
	switch ref.Kind {
	case KindChannel:
		var p postBusiness.PostPagination
		if before == nil {
			p, err = postBusiness.GetLatestPosts(ctx, ref.ID)
		} else {
			p, err = postBusiness.GetOldPosts(ctx, ref.ID, *before)
		}
		return Page{Posts: p.Posts, HasMore: p.HasMore}, err
	case KindDM:
		var c chatBusiness.ChatPagination
		switch them := ref.Other(me); {
		case before != nil:
			c, err = chatBusiness.GetDgraphOldChatFromDgraph(ctx, *before, me, them)
		default:
			var id uuid.UUID
			if id, err = uuid.Parse(me); err == nil {
				c, err = chatBusiness.GetDgraphLatestChatFromDgraph(ctx, id, them)
			}
		}
		return Page{Chats: c.Chats, HasMore: c.HasMore}, err
	case KindGroup:
		var c chatBusiness.ChatPagination
		if before != nil {
			c, err = chatBusiness.GetDgraphOldGroupChatFromDgraph(ctx, *before, ref.ID)
		} else {
			var id uuid.UUID
			if id, err = uuid.Parse(me); err == nil {
				c, err = chatBusiness.GetDgraphLatestGroupChatFromDgraph(ctx, id, ref.ID)
			}
		}
		return Page{Chats: c.Chats, HasMore: c.HasMore}, err
	}
	return Page{}, ErrNotFound
}
