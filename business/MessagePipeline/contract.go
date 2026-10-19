// Package business (MessagePipeline) is the one write path for a message
// (design/chat-vs-slack/PROGRAM.md §4.2 #23 and CH2; A2 builds it): validate,
// write, emit, with every side effect a sink. Every writer calls Messages.Write:
// people, bots, guests, the Slack bridge, Connect, the API and the importer.
//
// DAY 0. A send is today's send, exactly as /po/createPost, /dm/createChat and
// /groupChat/createChat make it: business/Send's rules (Prepare), then the
// write (Commit), once per client id (Send.Once), with today's side effects.
// An edit, a deletion, a reaction or a thread reply answers ErrUnavailable:
// their rules still live in the handlers (controllers/Post and Chat), and A2
// moves them here, behind this interface, when it makes those routes thin
// adapters. Nothing calls Write yet, so nothing changes by its existing.
package business

import (
	"context"
	"errors"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Op is what a write does.
type Op string

const (
	OpSend   Op = "send"
	OpEdit   Op = "edit"
	OpDelete Op = "delete"
	OpReact  Op = "react"
)

// Draft is a message's content as its writer sent it.
type Draft struct {
	HTML  string                           `json:"html"`
	Files []*dgraphStruct.DgraphAttachment `json:"files,omitempty"`
	// ReplyTo is a message in the same conversation this one quotes.
	ReplyTo string `json:"reply_to,omitempty"`
	// ClientMsgID is the id the app gave the message when it showed it: a
	// write tried again with it is the same write (CH3).
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// Write is one write to one conversation.
type Write struct {
	Op Op
	// To is the conversation (a channel, DM or group), or for a reply, the
	// thread (Kind thread, its root's id).
	To conversationBusiness.Ref
	// Message is the message an edit, deletion or reaction is to.
	Message string
	Draft   Draft
	// Emoji is a reaction's.
	Emoji string
}

// Written is the message a write made or changed.
type Written struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// Pipeline writes messages. A refusal is a *sendBusiness.Rejection, with the
// status and words the routes have always answered with.
type Pipeline interface {
	Write(ctx context.Context, who *userModels.UserInfo, w Write) (Written, error)
}

// ErrUnavailable is a write the default doesn't make yet: A2 brings it.
var ErrUnavailable = errors.New("messages: that change isn't available here yet")

// Messages is the pipeline every writer calls: the durable write for the
// surfaces MESSAGE_PIPELINE switches on, today's for the rest (durable.go).
var Messages Pipeline = durable{today: todaysWrites{}}

// todaysWrites sends as today's routes do.
type todaysWrites struct{}

func (todaysWrites) Write(ctx context.Context, who *userModels.UserInfo, w Write) (Written, error) {
	if w.Op != OpSend {
		return Written{}, ErrUnavailable
	}
	if who == nil {
		return Written{}, conversationBusiness.ErrNotFound
	}
	me := who.UserDgraphInfo.Uuid
	switch w.To.Kind {
	case conversationBusiness.KindChannel:
		in := channelInput(w)
		post, err := sendBusiness.PrepareChannelPost(ctx, who, in)
		if err != nil {
			return Written{}, err
		}
		out, err := sendBusiness.Once(ctx, me, onceKey(w, me), w.Draft.ClientMsgID, func() (*postAdapter.OutputCreatePostForPost, error) {
			return post.Commit(ctx)
		})
		if err != nil || out == nil {
			return Written{}, err
		}
		return Written{ID: out.Uuid, At: out.PostCreated}, nil
	case conversationBusiness.KindDM:
		in, ok := dmInput(w, me)
		if !ok {
			return Written{}, conversationBusiness.ErrNotFound
		}
		dm, err := sendBusiness.PrepareDirectMessage(ctx, who, in)
		if err != nil {
			return Written{}, err
		}
		return chatWritten(sendBusiness.Once(ctx, me, onceKey(w, me), w.Draft.ClientMsgID, func() (*chatAdapter.OutputCreateChatForChat, error) {
			return dm.Commit(ctx)
		}))
	case conversationBusiness.KindGroup:
		group, err := sendBusiness.PrepareGroupMessage(ctx, who, groupInput(w))
		if err != nil {
			return Written{}, err
		}
		return chatWritten(sendBusiness.Once(ctx, me, onceKey(w, me), w.Draft.ClientMsgID, func() (*chatAdapter.OutputCreateChatForChat, error) {
			return group.Commit(ctx)
		}))
	}
	// A reply in a thread: its rules are still the handlers'.
	return Written{}, ErrUnavailable
}

func chatWritten(out *chatAdapter.OutputCreateChatForChat, err error) (Written, error) {
	if err != nil || out == nil {
		return Written{}, err
	}
	return Written{ID: out.Uuid, At: out.ChatCreated}, nil
}

// channelInput is a channel send as /po/createPost takes it. Pure.
func channelInput(w Write) *postAdapter.InputCreateOrUpdatePostInfo {
	return &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText: w.Draft.HTML, MediaObj: w.Draft.Files, ChannelUuid: w.To.ID,
		ReplyToUUID: w.Draft.ReplyTo, ClientMsgID: w.Draft.ClientMsgID,
	}
}

// dmInput is a DM send as /dm/createChat takes it: to the other person the
// grouping id names beside me. ok is false when me isn't in it. Pure.
func dmInput(w Write, me string) (*chatAdapter.ChatInfo, bool) {
	to := w.To.Other(me)
	if to == "" {
		return nil, false
	}
	return &chatAdapter.ChatInfo{
		TextHtml: w.Draft.HTML, MediaObjects: w.Draft.Files, ToUuid: to,
		ReplyToUuid: w.Draft.ReplyTo, ClientMsgID: w.Draft.ClientMsgID,
	}, true
}

// groupInput is a group send as /groupChat/createChat takes it. Pure.
func groupInput(w Write) *chatAdapter.ChatInfo {
	return &chatAdapter.ChatInfo{
		TextHtml: w.Draft.HTML, MediaObjects: w.Draft.Files, GrpUuid: w.To.ID,
		ReplyToUuid: w.Draft.ReplyTo, ClientMsgID: w.Draft.ClientMsgID,
	}
}

// onceKey is the place a send is once in, as the routes name it, so a retry
// through the pipeline and one through a route are the same send. Pure.
func onceKey(w Write, me string) string {
	switch w.To.Kind {
	case conversationBusiness.KindChannel:
		return "ch:" + w.To.ID
	case conversationBusiness.KindDM:
		if in, ok := dmInput(w, me); ok {
			return "dm:" + in.ToUuid
		}
	case conversationBusiness.KindGroup:
		return "grp:" + w.To.ID
	}
	return ""
}
